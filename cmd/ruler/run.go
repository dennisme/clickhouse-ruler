package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/dennisme/clickhouse-ruler/internal/lint"
	"github.com/dennisme/clickhouse-ruler/internal/notify"
	"github.com/dennisme/clickhouse-ruler/internal/query"
	"github.com/dennisme/clickhouse-ruler/internal/ruleset"
	"github.com/dennisme/clickhouse-ruler/internal/scheduler"
)

// exitRun is a distinct code from exitUsage so an operator's process
// supervisor can tell "the flags were wrong" from "the ruler could not
// connect to a source it needs".
const exitRun = 3

// defaultShutdownTimeout is how long a running evaluation gets to finish
// once SIGINT or SIGTERM arrives, before the process gives up on it anyway.
const defaultShutdownTimeout = 30 * time.Second

func runRun(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	fs.SetOutput(stderr)

	rulesDir := fs.String("rules", "", "path to the rules directory (required)")
	sourcesPath := fs.String("sources", "sources.yaml", "path to the sources file")
	configPath := fs.String("config", "", "path to a policy file, defaults to ruler.yaml beside the rules directory if present")
	alertmanagerURL := fs.String("alertmanager", "", "Alertmanager URL, e.g. http://localhost:9093 (required)")
	listen := fs.String("listen", ":9090", "address for the /metrics, /-/healthy and /-/ready HTTP surface")
	queryConcurrency := fs.Int("query-concurrency", scheduler.DefaultQueryConcurrency,
		"how many rule queries may run against ClickHouse at once, across every group; 0 means unbounded")
	shutdownTimeout := fs.Duration("shutdown-timeout", defaultShutdownTimeout,
		"how long an in-flight evaluation gets to finish once shutdown starts")

	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if *rulesDir == "" || *alertmanagerURL == "" {
		printf(stderr, "%s\n", "usage: ruler run --rules <dir> --alertmanager <url> [flags]")
		return exitUsage
	}

	sources, problems, err := loadSources(*sourcesPath)
	if err != nil {
		printf(stderr, "%s\n", err)
		return exitUsage
	}

	root, configProblems, err := loadPolicy(*configPath, *rulesDir)
	if err != nil {
		printf(stderr, "%s\n", err)
		return exitUsage
	}
	problems = append(problems, configProblems...)

	set, ruleProblems := ruleset.Load(*rulesDir, sources, root)
	problems = append(problems, ruleProblems...)

	if err := lint.Format(stderr, lint.FormatText, problems); err != nil {
		printf(stderr, "%s\n", err)
		return exitUsage
	}

	// An error-severity finding refuses to start; a warning is logged above
	// and the ruler runs anyway (spec 7.6).
	for _, p := range problems {
		if p.Severity == lint.SeverityError {
			printf(stderr, "refusing to start: at least one rule failed a correctness check\n")
			return exitFinding
		}
	}

	queriers, err := openQueriers(set)
	if err != nil {
		printf(stderr, "%s\n", err)
		return exitRun
	}
	defer closeQueriers(queriers)

	reg := prometheus.NewRegistry()
	metrics := scheduler.NewMetrics(reg)
	clock := scheduler.NewRealClock()

	client := notify.NewClient(*alertmanagerURL)
	cadence := scheduler.NewCadence(client, *alertmanagerURL, notify.DefaultResendInterval, metrics, clock)

	sched := scheduler.New(set, toQuerierMap(queriers), cadence, metrics, clock, *queryConcurrency)

	httpSrv := &http.Server{Addr: *listen, Handler: scheduler.Handler(reg), ReadHeaderTimeout: 5 * time.Second}
	go func() {
		if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			printf(stderr, "http server: %s\n", err)
		}
	}()

	sched.Start(ctx)
	printf(stdout, "ruler running, %d rule(s) loaded, listening on %s\n", len(set.Rules), *listen)

	<-ctx.Done()
	printf(stdout, "shutting down\n")
	sched.Shutdown(*shutdownTimeout)

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = httpSrv.Shutdown(shutdownCtx)

	return exitOK
}

// openQueriers connects to every source any loaded rule matched, once each,
// so a rule that matches several sources shares a connection with any other
// rule matching the same one.
func openQueriers(set *ruleset.Set) (map[string]*query.Querier, error) {
	out := map[string]*query.Querier{}
	for _, r := range set.Rules {
		for _, src := range r.Sources {
			if _, ok := out[src.Name]; ok {
				continue
			}
			q, err := query.Open(src)
			if err != nil {
				closeQueriers(out)
				return nil, fmt.Errorf("opening source %q: %w", src.Name, err)
			}
			out[src.Name] = q
		}
	}
	return out, nil
}

func closeQueriers(queriers map[string]*query.Querier) {
	for _, q := range queriers {
		_ = q.Close()
	}
}

func toQuerierMap(queriers map[string]*query.Querier) map[string]scheduler.Querier {
	out := make(map[string]scheduler.Querier, len(queriers))
	for name, q := range queriers {
		out[name] = q
	}
	return out
}
