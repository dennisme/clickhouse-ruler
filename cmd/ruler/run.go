package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/dennisme/clickhouse-ruler/internal/notify"
	"github.com/dennisme/clickhouse-ruler/internal/query"
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

	reg := prometheus.NewRegistry()
	rn := &runner{
		rulesDir:    *rulesDir,
		sourcesPath: *sourcesPath,
		configPath:  *configPath,
		log:         log,
		stderr:      stderr,
		metrics:     scheduler.NewMetrics(reg),
		clock:       scheduler.NewRealClock(),
		concurrency: *queryConcurrency,
		// Both the cadence and each rule's resolved-alert retention are sized
		// from these two, so they travel as the pair they are (spec 6.5).
		resend: scheduler.Resend{Interval: *resendInterval, Tolerance: *resendTolerance},
	}
	rn.cadence = scheduler.NewCadence(notify.NewClient(*alertmanagerURL), *alertmanagerURL,
		rn.resend, rn.metrics, rn.clock)

	// SIGHUP is the whole trigger. Nothing watches the filesystem: an operator
	// or whatever rolled the files out says when they are complete, and a
	// watcher would read a rules tree half way through being written.
	//
	// Registered before the first load, because the default disposition of
	// SIGHUP is to terminate: a signal arriving while the ruler is still
	// connecting to twelve clusters would otherwise kill it, which is a
	// confusing way to learn that a reload is only accepted once startup
	// finished.
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	defer signal.Stop(hup)

	cfg, err := rn.load()
	if err != nil {
		printf(stderr, "%s\n", err)
		return exitUsage
	}
	rn.report(cfg.problems)

	// An error-severity finding refuses to start; a warning is reported above
	// and the ruler runs anyway (spec 7.6).
	if cfg.refused() {
		printf(stderr, "refusing to start: at least one rule failed a correctness check\n")
		return exitFinding
	}

	if err := rn.connect(ctx, cfg); err != nil {
		printf(stderr, "%s\n", err)
		return exitRun
	}
	defer rn.close()

	rn.build(cfg)

	httpSrv := &http.Server{
		Addr:              *listen,
		Handler:           scheduler.Handler(reg, rn.ready),
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Error("metrics listener stopped", "listen", *listen, "error", err.Error())
		}
	}()

	rn.sched.Start(ctx)
	log.Info("ruler running", "rules", len(cfg.set.Rules), "listen", *listen)

	for running := true; running; {
		select {
		case <-ctx.Done():
			running = false
		case <-hup:
			rn.reload(ctx)
		}
	}

	log.Info("shutting down", "timeout", shutdownTimeout.String())
	rn.sched.Shutdown(*shutdownTimeout)

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = httpSrv.Shutdown(shutdownCtx)

	return exitOK
}

// queryCost reports what an evaluation's query cost into the metrics
// registry (spec 8.2).
//
// The adapter exists so internal/query never learns what a Prometheus
// collector is and internal/scheduler never learns what a driver callback
// is. It is the wiring, so it lives where the wiring is.
type queryCost struct{ metrics *scheduler.Metrics }

func (c queryCost) QueryCost(rule, team string, u query.Usage) {
	c.metrics.QueryReadRowsTotal.WithLabelValues(rule, team).Add(float64(u.ReadRows))
	c.metrics.QueryReadBytesTotal.WithLabelValues(rule, team).Add(float64(u.ReadBytes))
	c.metrics.QueryDuration.WithLabelValues(rule).Observe(u.Duration.Seconds())

	// A query the server reported no memory for is one that never ran, such
	// as a connection that failed. Observing a zero would pull the histogram
	// down with a measurement nobody made.
	if u.PeakMemory > 0 {
		c.metrics.QueryMemoryUsage.WithLabelValues(rule).Observe(float64(u.PeakMemory))
	}
}

// pinger is all readiness asks of a source: whether it answers.
// query.Querier is one.
type pinger interface {
	Ping(ctx context.Context) error
}

// readiness answers whether sending traffic to this ruler is useful: rules
// loaded, and at least one source answering (spec 8.1).
//
// Not a source-by-source answer. One unreachable cluster out of twelve is a
// finding for source/privileges and the evaluation failure counters, not a
// reason to declare the whole ruler unfit, and a probe that flaps with any
// cluster's availability is one whoever is on call disables.
func readiness(rules int, sources map[string]pinger) scheduler.Ready {
	return func(ctx context.Context) error {
		if rules == 0 {
			return errors.New("no rules loaded, so there is nothing to evaluate")
		}
		for _, src := range sources {
			if err := src.Ping(ctx); err == nil {
				return nil
			}
		}
		return errors.New("no source answering, so every rule this ruler holds fails to evaluate")
	}
}

func toPingers(queriers map[string]*query.Querier) map[string]pinger {
	out := make(map[string]pinger, len(queriers))
	for name, q := range queriers {
		out[name] = q
	}
	return out
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
