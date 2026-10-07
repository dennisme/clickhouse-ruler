// Command ruler validates and evaluates ClickHouse alert rules.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/dennisme/clickhouse-ruler/internal/buildinfo"
	"github.com/dennisme/clickhouse-ruler/internal/cli"
	"github.com/dennisme/clickhouse-ruler/internal/lint"
	"github.com/dennisme/clickhouse-ruler/internal/policy"
	"github.com/dennisme/clickhouse-ruler/internal/ruleset"
	"github.com/dennisme/clickhouse-ruler/internal/source"
)

// Exit codes. Findings and operational failures are separated so CI can tell
// "your rules are wrong" from "the tool could not run".
const (
	exitOK      = 0
	exitFinding = 1
	exitUsage   = 2
)

// Two output streams, two audiences.
//
// Usage errors, lint findings and the refusal to start are CLI output: a
// person ran a command and the command has something to say about what they
// typed. Those go to stderr through printf below, unstructured, and they are
// not log lines.
//
// Everything `ruler run` says once it is running is a log line, and goes to
// stdout through slog, where a process supervisor collects it. That is where
// the daemon's lifetime events already went before there was a logger.
//
// printf writes diagnostic output. A write to stdout or stderr that fails
// leaves nowhere to report the failure, so the error is deliberately dropped
// rather than checked at every call site.
func printf(w io.Writer, format string, args ...any) {
	_, _ = fmt.Fprintf(w, format, args...)
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		printf(stderr, "%s\n", "usage: ruler <check|run|version> [flags]")
		return exitUsage
	}

	switch args[0] {
	case "check":
		return check(args[1:], stdout, stderr)
	case "run":
		ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
		defer stop()
		return runRun(ctx, args[1:], stdout, stderr)
	case "version":
		printf(stdout, "%s", buildinfo.Get())
		return exitOK
	default:
		printf(stderr, "unknown command %q, want check, run or version\n", args[0])
		return exitUsage
	}
}

func check(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("check", flag.ContinueOnError)
	fs.SetOutput(stderr)

	opts := cli.CheckFlags(fs)

	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if fs.NArg() < 1 {
		printf(stderr, "%s\n", "usage: ruler check [flags] <rules-dir> [path...]")
		return exitUsage
	}
	dir := fs.Arg(0)

	if err := lint.Format(io.Discard, *opts.Format, nil); err != nil {
		printf(stderr, "%s\n", err)
		return exitUsage
	}

	// stdout in every format but text belongs to a machine: the workflow runner
	// parses each line as a command and json is one document, so a table there
	// is a stray annotation per row or a document that will not parse.
	//
	// One answer for both tables, and asked before anything else either flag
	// demands: a run that would corrupt stdout is refused whether or not the
	// cluster the cost table needs could have been reached.
	for _, dest := range []struct{ flag, path string }{
		{"--markdown", *opts.Markdown},
		{"--summary", *opts.Summary},
	} {
		if dest.path == "-" && *opts.Format != lint.FormatText {
			printf(stderr, "%s - needs --format=%s: stdout already carries the %s output\n",
				dest.flag, lint.FormatText, *opts.Format)
			return exitUsage
		}
	}

	// The table's numbers come from EXPLAIN ESTIMATE, which needs a cluster
	// to ask. Offline there is nothing to put in it, and an empty table would
	// read as an estate where every rule is free (spec 7.10).
	if *opts.Summary != "" && !*opts.Online && !*opts.Sample && !*opts.Backfill {
		printf(stderr, "%s\n", "--summary needs --online: the cost of a rule is a question for the cluster it runs on")
		return exitUsage
	}

	sources, problems, err := loadSources(*opts.ConfigPath)
	if err != nil {
		printf(stderr, "%s\n", err)
		return exitUsage
	}

	root, policyProblems, err := loadPolicy(*opts.PolicyPath, dir)
	if err != nil {
		printf(stderr, "%s\n", err)
		return exitUsage
	}
	problems = append(problems, policyProblems...)

	set, ruleProblems := ruleset.Load(dir, sources, root)
	problems = append(problems, ruleProblems...)

	// Resolved before the online checks rather than beside the narrowing
	// below, so a mistyped path costs nothing: it is the one filter that can
	// fail, and failing after connecting to every cluster would charge an
	// author for a typo (spec 10.3).
	paths, err := lint.Paths(set.Files, fs.Args()[1:])
	if err != nil {
		printf(stderr, "%s\n", err)
		return exitUsage
	}

	// Reading rows implies a connection, whether once or once per window: the
	// checks that read them need the columns and types the metadata checks
	// resolve, so asking for one without the other would leave nothing to read
	// against (spec 7.3). Neither implies the other, because a replay is a
	// larger read than a sample and is consented to on its own.
	if *opts.Online || *opts.Sample || *opts.Backfill {
		ctx := context.Background()

		// Every source in the file, not only the ones a rule matched. The
		// finding belongs to the pull request that changed the sources file,
		// in front of the people who own it (spec 6.7.3).
		problems = append(problems, checkPrivileges(ctx, *opts.ConfigPath, sources.Sources, root)...)

		// Rules are the other way round: only the sources they matched, since
		// a rule is read through the cluster it will run on.
		inspected, rows := inspectRules(ctx, set, inspectOptions{
			sampling:      *opts.Sample,
			backfilling:   *opts.Backfill,
			summarising:   *opts.Summary != "",
			backfillRange: *opts.BackfillRange,
			backfillStep:  *opts.BackfillStep,
		})
		problems = append(problems, inspected...)

		if *opts.Summary != "" {
			if err := writeSummary(*opts.Summary, rows, stdout); err != nil {
				printf(stderr, "%s\n", err)
				return exitUsage
			}
		}
	}

	// After every check has run, because the loader walks the whole tree and
	// the checks about a pair or a tree are answers about the tree. Narrowing
	// here can only drop a finding a full run would also have reported, and
	// the exit code below follows what is left (spec 10.3).
	if *opts.ChangedSince != "" {
		filter := lint.ChangedSince(dir, *opts.ChangedSince, []string{*opts.ConfigPath, *opts.PolicyPath})
		if filter.Note != "" {
			printf(stderr, "%s\n", filter.Note)
		}
		problems = filter.Keep(problems)
	}

	// Both filters apply when both were asked for, which is the changed files
	// among the paths named. A ChangedSince that widened to everything does
	// not widen past a path, because that path was asked for explicitly
	// (spec 10.3).
	problems = paths.Keep(problems)

	// Written before the log format, so a workflow gets the annotations and the
	// comment body from one run of the checks rather than two (spec 10.3).
	if *opts.Markdown != "" {
		if err := writeReport(*opts.Markdown, *opts.LinkPrefix, problems, stdout); err != nil {
			printf(stderr, "%s\n", err)
			return exitUsage
		}
	}

	if err := lint.Format(stdout, *opts.Format, problems); err != nil {
		printf(stderr, "%s\n", err)
		return exitUsage
	}
	if *opts.Explain {
		// stdout belongs to a machine in every format but text: the workflow
		// runner parses each line as a command, and json is one document. The
		// explanation is for a human, so it goes to stderr rather than
		// becoming stray annotations on the diff or breaking the parse.
		out := stdout
		if *opts.Format != lint.FormatText {
			out = stderr
		}
		explainSet(out, set)
	}

	for _, p := range problems {
		if p.Severity == lint.SeverityError {
			return exitFinding
		}
	}
	return exitOK
}

func loadSources(path string) (*source.File, []lint.Problem, error) {
	data, err := os.ReadFile(path) //nolint:gosec // an operator-supplied path is the input
	if err != nil {
		return nil, nil, fmt.Errorf("reading sources: %w", err)
	}
	f, problems := source.Parse(path, data, nil)

	// Expiry is state rather than shape, so it is read here with a clock
	// rather than at parse time. Both commands load sources through this, so
	// an exemption that has run out fails CI and refuses to start.
	now := time.Now()
	problems = append(problems, f.ExpiredExemptions(now)...)

	// Read with the same clock and for the same reason: whether a verification
	// downgrade is still agreed to is state, and the exemption that clears it
	// has a date (spec 6.2, 7.7).
	problems = append(problems, f.InsecureTLS(now)...)

	return f, problems, nil
}

// loadPolicy reads the policy file. An explicit path that does not exist is an
// error, because an operator who passed --policy meant it. The implicit
// policy.yaml beside the rules is optional, because most repositories will not
// have one and the defaults are meant to work.
func loadPolicy(path, dir string) (*policy.Policy, []lint.Problem, error) {
	explicit := path != ""
	if !explicit {
		path = filepath.Join(dir, "policy.yaml")
	}

	data, err := os.ReadFile(path) //nolint:gosec // an operator-supplied path is the input
	switch {
	case err == nil:
	case explicit:
		return nil, nil, fmt.Errorf("reading policy: %w", err)
	default:
		// An empty policy rather than the defaults: For falls back to them
		// anyway, and passing them as a scope would make every default
		// severity a floor that a source's own policy could not turn off.
		return &policy.Policy{}, nil, nil
	}

	p, problems := policy.Parse(path, data)
	return p, problems, nil
}

// explainSet prints the resolved policy per rule, naming the file that set
// each severity so an author can see why a check blocks them instead of
// guessing which of several files is responsible (spec 7.8).
func explainSet(w io.Writer, set *ruleset.Set) {
	// The scopes the rule's location decided and the sources it matched
	// are both already resolved on a loaded rule, so this is the same
	// merge the loader validated against.
	for _, r := range set.Rules {
		scopes := []*policy.Policy{r.Policy}
		for _, src := range r.Sources {
			scopes = append(scopes, src.Policy)
		}
		merged := policy.Merge(scopes...)

		printf(w, "\n%s: %s\n", r.File, r.Alert)

		// Which clusters a rule runs against is no longer readable from the
		// rule itself: labels decide it and the answer can be more than one.
		if len(r.Sources) == 0 {
			printf(w, "  sources: none matched, this ruler will not evaluate it\n")
		} else {
			for _, src := range r.Sources {
				printf(w, "  source   %s (%s)\n", src.Name, src.Address)

				// An exemption is the one thing that drops a finding rather
				// than raising one, so it has to be readable here or a check
				// that stopped reporting looks like a check that passed.
				for _, e := range src.Exemptions {
					printf(w, "    exempt %s until %s: %s\n",
						e.Check, e.Until.Format(time.DateOnly), e.Reason)
				}
			}
		}

		// Every configurable check, not only the ones a file mentioned: an
		// author asking why a check blocks them is not helped by a list that
		// omits the checks nobody configured.
		for _, name := range lint.Configurables() {
			s := merged.For(name)
			origin := "default"
			if s.File != "" {
				origin = fmt.Sprintf("%s:%d", s.File, s.Line)
			}
			printf(w, "  %-24s %-7s from %s", name, s.Severity, origin)
			if len(s.Keys) > 0 {
				printf(w, " keys=%v", s.Keys)
			}
			printf(w, "\n")
		}
	}
}
