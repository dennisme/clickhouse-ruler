package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
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

// parseLogLevel reads the --log-level flag. An unparseable level is refused
// rather than defaulted, because an operator who asked for debug output and
// got none has no way to tell why.
func parseLogLevel(s string) (slog.Level, error) {
	var level slog.Level
	if err := level.UnmarshalText([]byte(s)); err != nil {
		return 0, fmt.Errorf("--log-level %q: want debug, info, warn or error", s)
	}
	return level, nil
}

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
	resendInterval := fs.Duration("resend-interval", notify.DefaultResendInterval,
		"how often a still-firing alert is re-posted to Alertmanager; each alert is sent an expiry of four times this")
	resendTolerance := fs.Int("resend-tolerance", notify.DefaultResendTolerance,
		"how many resend periods a firing alert stays valid for, so how many consecutive failed evaluations or sends "+
			"pass before Alertmanager expires an alert that is still firing; 4 is what Prometheus gives itself")
	logLevel := fs.String("log-level", "info", "log verbosity: debug, info, warn or error")

	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if *rulesDir == "" || *alertmanagerURL == "" {
		printf(stderr, "%s\n", "usage: ruler run --rules <dir> --alertmanager <url> [flags]")
		return exitUsage
	}
	// A non-positive interval makes every firing alert due on every
	// evaluation and gives it an expiry that has already passed, which
	// Alertmanager reads as resolved.
	if *resendInterval <= 0 {
		printf(stderr, "--resend-interval must be positive, got %s\n", *resendInterval)
		return exitUsage
	}

	// One period of validity expires a firing alert at the exact moment it is
	// next due, leaving no room for the send that would have renewed it to
	// fail. Tolerating nothing is not a tolerance.
	if *resendTolerance < 2 {
		printf(stderr, "--resend-tolerance must be at least 2, got %d\n", *resendTolerance)
		return exitUsage
	}

	level, err := parseLogLevel(*logLevel)
	if err != nil {
		printf(stderr, "%s\n", err)
		return exitUsage
	}
	log := slog.New(slog.NewTextHandler(stdout, &slog.HandlerOptions{Level: level}))

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

	// Both the cadence and each rule's resolved-alert retention are sized from
	// these two, so they are passed as the pair they are (spec 6.5).
	resend := scheduler.Resend{Interval: *resendInterval, Tolerance: *resendTolerance}

	client := notify.NewClient(*alertmanagerURL)
	cadence := scheduler.NewCadence(client, *alertmanagerURL, resend, metrics, clock)

	sched := scheduler.New(set, toQuerierMap(queriers), cadence, metrics, clock, *queryConcurrency, log, resend)

	httpSrv := &http.Server{Addr: *listen, Handler: scheduler.Handler(reg), ReadHeaderTimeout: 5 * time.Second}
	go func() {
		if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Error("metrics listener stopped", "listen", *listen, "error", err.Error())
		}
	}()

	sched.Start(ctx)
	log.Info("ruler running", "rules", len(set.Rules), "listen", *listen)

	<-ctx.Done()
	log.Info("shutting down", "timeout", shutdownTimeout.String())
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
