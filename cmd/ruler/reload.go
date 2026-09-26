package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"reflect"
	"sync"

	"github.com/dennisme/clickhouse-ruler/internal/lint"
	"github.com/dennisme/clickhouse-ruler/internal/notify"
	"github.com/dennisme/clickhouse-ruler/internal/policy"
	"github.com/dennisme/clickhouse-ruler/internal/query"
	"github.com/dennisme/clickhouse-ruler/internal/ruleset"
	"github.com/dennisme/clickhouse-ruler/internal/scheduler"
	"github.com/dennisme/clickhouse-ruler/internal/source"
)

// config is one reading of the three files: the rules tree, the sources file
// and the policy. It is what both startup and a reload produce, because both go
// through the same loader and the same checks. Validation has one entry point
// for the ruler and one for CI, and a reload is neither a third nor a shortcut
// past the first (spec 7.1).
type config struct {
	set      *ruleset.Set
	root     *policy.Policy
	problems []lint.Problem
}

// refused reports whether these files may not run. An error-severity finding
// refuses the whole reading: at startup that is a refusal to start, and on a
// reload it keeps the version already running (spec 7.6).
func (c *config) refused() bool {
	for _, p := range c.problems {
		if p.Severity == lint.SeverityError {
			return true
		}
	}
	return false
}

// runner is what `ruler run` holds for the life of the process, as opposed to
// what one reading of the files produces.
//
// The split is the point. The metrics registry, the logger, the clock, the
// notify.Cadence and the scheduler are the process; the rules, the sources and
// the policy are a version of some files, and SIGHUP replaces that version
// without replacing any of the rest. The Cadence matters most here: it holds
// when every firing alert was last posted, so a reload that rebuilt it would
// re-post every firing alert at once and give each a fresh resend interval
// (spec 6.5).
type runner struct {
	rulesDir    string
	sourcesPath string
	configPath  string

	log     *slog.Logger
	stderr  io.Writer
	metrics *scheduler.Metrics
	clock   scheduler.Clock
	cadence *notify.Cadence

	concurrency int
	resend      scheduler.Resend

	// sched is nil until build, so connect can be the one place that opens
	// connections for both startup and a reload.
	sched *scheduler.Scheduler

	// mu guards what a reload replaces. The readiness probe reads it from an
	// HTTP handler while a reload is writing it, which is the only concurrent
	// reader: reloads themselves arrive one signal at a time on the same
	// goroutine.
	mu       sync.Mutex
	queriers map[string]*query.Querier

	// sources is the definition behind each open connection, so a reload can
	// tell a source that merely stayed matched from one whose file changed.
	sources map[string]source.Source

	// rules is how many rules the running configuration holds, which is what
	// readiness answers "nothing to evaluate" from (spec 8.1).
	rules int
}

// load reads the three files and validates them, which is exactly what
// `ruler check` does to the same tree. One path, so a rule that would fail CI
// cannot be loaded by a reload either (spec 7.1).
func (r *runner) load() (*config, error) {
	sources, problems, err := loadSources(r.sourcesPath)
	if err != nil {
		return nil, err
	}

	root, configProblems, err := loadPolicy(r.configPath, r.rulesDir)
	if err != nil {
		return nil, err
	}
	problems = append(problems, configProblems...)

	set, ruleProblems := ruleset.Load(r.rulesDir, sources, root)
	problems = append(problems, ruleProblems...)

	return &config{set: set, root: root, problems: problems}, nil
}

// report writes the findings where a person will read them.
//
// On stderr rather than through the logger, and for a reload as much as for
// startup. A finding is CLI output: it carries a file, a line and a severity
// and it is formatted to be read by whoever is holding the file (spec 7.3).
// An operator who sent SIGHUP is reading the stream they started the ruler on,
// and folding the findings into log lines would make the same finding look
// different depending on which of the two produced it.
func (r *runner) report(problems []lint.Problem) {
	if err := lint.Format(r.stderr, lint.FormatText, problems); err != nil {
		printf(r.stderr, "%s\n", err)
	}
}

// connect reconciles the open connections with what cfg needs, hands the new
// configuration to the scheduler, and closes whatever is left over.
//
// The order is what makes closing safe. A connection is opened for every source
// the new configuration reaches before the scheduler is told anything, the
// scheduler swaps configurations and does not return until every evaluation of
// the previous one has finished, and only then is a connection closed. So a
// querier is only ever closed once nothing holds it: the evaluations that could
// have been using it have returned, and the evaluations that start next were
// handed the map this function just built.
//
// The contract check runs first, before any connection is opened, which is the
// same order startup uses and for the same reason: a source refused at error
// severity is never connected to at all. A reload re-checks it deliberately.
// 6.7.3 asks for the contract to be checked at `ruler check`, at startup and on
// a reload, and names the window it leaves open: a grant revoked while the ruler
// runs is not noticed until one of those happens. A reload is the moment an
// operator has just changed the files, so it is the cheapest of the three to be
// honest at, and skipping it would mean a ruler that has been up for a month has
// never re-read the only report there is of a boundary that has moved. The cost
// is a handful of statements per source, each refused before it does any work,
// and never one per evaluation.
func (r *runner) connect(ctx context.Context, cfg *config) error {
	if refused := refusedSources(ctx, r.sourcesPath, cfg.set, cfg.root, r.stderr, r.log); len(refused) > 0 {
		refuseSources(cfg.set, refused)
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	next := map[string]*query.Querier{}
	opened := map[string]*query.Querier{}
	definitions := map[string]source.Source{}

	for _, src := range matchedSources(cfg.set) {
		definitions[src.Name] = src

		// The whole definition, not the address and the credentials alone. A
		// query.Querier keeps the source it was opened with and sends that
		// source's caps, table and timestamp column with every query, so any
		// field that changed is a field the running connection would still be
		// using the old value of. A list of which fields matter would be
		// wrong the first time one is added.
		if held, ok := r.queriers[src.Name]; ok && reflect.DeepEqual(r.sources[src.Name], src) {
			next[src.Name] = held
			continue
		}

		q, err := query.Open(src, queryCost{r.metrics})
		if err != nil {
			closeQueriers(opened)
			return fmt.Errorf("opening source %q: %w", src.Name, err)
		}
		opened[src.Name], next[src.Name] = q, q
	}

	if r.sched != nil {
		r.sched.Reload(cfg.set, toQuerierMap(next))
	}

	for name, q := range r.queriers {
		if next[name] != q {
			_ = q.Close()
		}
	}

	r.queriers, r.sources, r.rules = next, definitions, len(cfg.set.Rules)
	r.metrics.ConfigLastReloadSuccessful.Set(1)
	r.metrics.ConfigLastReloadTimestamp.Set(float64(r.clock.Now().Unix()))
	return nil
}

// build creates the scheduler around the configuration connect has already
// opened connections for. Called once, at startup: every later configuration
// reaches the same scheduler through Reload, because the alert state and the
// group goroutines are the process rather than the files.
func (r *runner) build(cfg *config) {
	r.sched = scheduler.New(cfg.set, toQuerierMap(r.queriers), r.cadence,
		r.metrics, r.clock, r.concurrency, r.log, r.resend)
}

// reload re-reads the files and replaces what is running with them.
//
// Every way this can fail leaves the ruler evaluating what it was already
// evaluating, and says so on clickhouse_ruler_config_last_reload_successful.
// That is the only signal a refused reload produces: the rules that are running
// are valid, they evaluate, they deliver, and nothing about them looks wrong.
// The file on disk saying something else is invisible from the outside, which is
// why the gauge is the alert an operator is expected to have (spec 8.2).
func (r *runner) reload(ctx context.Context) {
	r.log.Info("reloading", "rules", r.rulesDir, "sources", r.sourcesPath)

	cfg, err := r.load()
	if err != nil {
		r.refuse("a file could not be read", err)
		return
	}

	r.report(cfg.problems)

	// A warning is logged by the report above and the reload proceeds; an error
	// refuses the whole reading, including the files in it that are fine, because
	// a rules tree is loaded as a tree and half of one is not a configuration
	// anybody wrote down (spec 7.6).
	if cfg.refused() {
		r.refuse("at least one rule failed a correctness check", nil)
		return
	}

	if err := r.connect(ctx, cfg); err != nil {
		r.refuse("a source could not be opened", err)
		return
	}

	r.mu.Lock()
	rules, sources := r.rules, len(r.queriers)
	r.mu.Unlock()
	r.log.Info("reloaded", "rules", rules, "sources", sources)
}

// refuse records a reload that did not happen. The timestamp gauge is left
// where it is on purpose: it dates the configuration being evaluated, and a
// refused reload did not change that (see NewMetrics).
func (r *runner) refuse(reason string, err error) {
	r.metrics.ConfigLastReloadSuccessful.Set(0)

	args := []any{"reason", reason}
	if err != nil {
		args = append(args, "error", err.Error())
	}
	r.log.Error("refusing the reload, the previous configuration keeps running", args...)
}

// ready is the readiness probe over whatever configuration is loaded now. Read
// under the lock because a reload replaces both numbers it reads and the probe
// arrives on an HTTP handler's goroutine (spec 8.1).
func (r *runner) ready(ctx context.Context) error {
	r.mu.Lock()
	rules, sources := r.rules, toPingers(r.queriers)
	r.mu.Unlock()

	return readiness(rules, sources)(ctx)
}

// close releases every connection the runner holds, for shutdown.
func (r *runner) close() {
	r.mu.Lock()
	defer r.mu.Unlock()

	closeQueriers(r.queriers)
	r.queriers = nil
}
