// Package cli registers the flags each command takes.
//
// It exists so that the flag set is readable by something other than the
// command that parses it. The flag reference on the site is generated from
// these, the way the check pages are generated from the check table, and a
// generator cannot import `package main` (spec 14). The command bodies stay
// in cmd/ruler; what lives here is the registration and nothing else.
package cli

import (
	"flag"
	"strings"
	"time"

	"github.com/dennisme/clickhouse-ruler/internal/lint"
	"github.com/dennisme/clickhouse-ruler/internal/notify"
	"github.com/dennisme/clickhouse-ruler/internal/query"
	"github.com/dennisme/clickhouse-ruler/internal/scheduler"
)

// RunOptions is what `ruler run` was asked for, as the pointers flag.FlagSet
// fills in at Parse.
type RunOptions struct {
	RulesDir   *string
	ConfigPath *string
	PolicyPath *string
	Listen     *string

	QueryConcurrency *int
	RecheckInterval  *time.Duration
	ShutdownTimeout  *time.Duration

	ResendInterval  *time.Duration
	ResendTolerance *int
	QueueCapacity   *int

	LogLevel  *string
	LogFormat *string

	ReloadEndpoint *bool
}

// CheckOptions is what `ruler check` was asked for.
type CheckOptions struct {
	ConfigPath *string
	PolicyPath *string

	Format       *string
	ChangedSince *string
	Explain      *bool

	Online        *bool
	Sample        *bool
	Backfill      *bool
	BackfillRange *time.Duration
	BackfillStep  *time.Duration

	Markdown   *string
	LinkPrefix *string
	Summary    *string
}

// RunFlags registers the flags `ruler run` takes on fs.
func RunFlags(fs *flag.FlagSet) *RunOptions {
	o := &RunOptions{}

	o.RulesDir = fs.String("rules", "", "path to the rules directory (required)")
	o.ConfigPath = fs.String("config", "ruler.yaml", "path to the operator's file, which names the sources")
	o.PolicyPath = fs.String("policy", "", "path to a policy file, defaults to policy.yaml beside the rules directory if present")
	o.Listen = fs.String("listen", ":9090", "address for the /metrics, /-/healthy and /-/ready HTTP surface")
	o.QueryConcurrency = fs.Int("query-concurrency", scheduler.DefaultQueryConcurrency,
		"how many rule queries may run against ClickHouse at once, across every group; 0 means unbounded")
	o.RecheckInterval = fs.Duration("recheck-interval", scheduler.DefaultRecheckInterval,
		"how often loaded rules are re-checked against recent data for the map keys they read, "+
			"which no evaluation can see; 0 turns the pass off")
	o.ShutdownTimeout = fs.Duration("shutdown-timeout", DefaultShutdownTimeout,
		"how long an in-flight evaluation gets to finish once shutdown starts")
	o.ResendInterval = fs.Duration("resend-interval", notify.DefaultResendInterval,
		"how often a still-firing alert is re-posted to Alertmanager; each alert is sent an expiry of four times this")
	o.ResendTolerance = fs.Int("resend-tolerance", notify.DefaultResendTolerance,
		"how many resend periods a firing alert stays valid for, so how many consecutive failed evaluations or sends "+
			"pass before Alertmanager expires an alert that is still firing; 4 is what Prometheus gives itself")
	o.QueueCapacity = fs.Int("notification-queue-capacity", scheduler.DefaultNotificationQueueCapacity,
		"how many alerts may wait to be sent to Alertmanager before the oldest are dropped; the send runs off the "+
			"evaluation goroutine, so this is what an Alertmanager outage fills instead of a group's interval")
	o.LogLevel = fs.String("log-level", "info", "log verbosity: debug, info, warn or error")
	o.LogFormat = fs.String("log-format", "text", "log encoding: text or json")
	o.ReloadEndpoint = fs.Bool("enable-reload-endpoint", false,
		"serve POST /-/reload, which re-reads the same files SIGHUP does, for deployments where a signal cannot "+
			"reach the process")

	return o
}

// CheckFlags registers the flags `ruler check` takes on fs.
func CheckFlags(fs *flag.FlagSet) *CheckOptions {
	o := &CheckOptions{}

	o.ConfigPath = fs.String("config", "ruler.yaml", "path to the operator's file, which names the sources")
	o.PolicyPath = fs.String("policy", "", "path to a policy file, defaults to policy.yaml beside the rules directory if present")
	o.Format = fs.String("format", lint.FormatText,
		"output format: "+strings.Join(lint.Formats, ", "))
	o.ChangedSince = fs.String("changed-since", "",
		"only report findings in files that differ from the merge base with this git reference")
	o.Explain = fs.Bool("explain", false, "print each rule's resolved policy and where every setting came from")
	o.Online = fs.Bool("online", false,
		"also run the checks that need a ClickHouse connection, connecting as each source's own user")
	o.Sample = fs.Bool("sample", false,
		"also run the checks that read rows, which implies --online")
	o.Backfill = fs.Bool("backfill", false,
		"also replay each rule over a past range and report how many alerts it would have produced, "+
			"which reads rows once per window and implies --online")
	o.BackfillRange = fs.Duration("backfill-range", query.DefaultBackfillRange,
		"how far back --backfill reaches")
	o.BackfillStep = fs.Duration("backfill-step", 0,
		"the gap between the evaluations --backfill replays, defaulting to the rule's group interval")
	o.Markdown = fs.String("markdown", "",
		"write the findings as a markdown table to this path, - for stdout, for a pull request comment")
	o.LinkPrefix = fs.String("link-prefix", "",
		"URL a finding's path is appended to in the markdown table, such as "+
			"https://github.com/owner/repo/blob/<commit>/, which links each finding to its line")
	o.Summary = fs.String("summary", "",
		"write a markdown table of what each rule reads to this path, - for stdout, which needs --online")

	return o
}

// DefaultShutdownTimeout is how long a running evaluation gets to finish once
// SIGINT or SIGTERM arrives, before the process gives up on it anyway.
const DefaultShutdownTimeout = 30 * time.Second

// Command is one subcommand's flags, for anything reading the flags rather
// than parsing them.
type Command struct {
	Name  string
	Flags *flag.FlagSet
}

// Commands returns a registered flag set per command, in the order the
// reference documents them.
//
// Built here rather than handed in, so a command that grows a flag grows a row
// without anybody remembering to tell the generator. flag.ContinueOnError
// matches what the commands use, and nothing here parses, so the error mode
// never comes up.
func Commands() []Command {
	run := flag.NewFlagSet("run", flag.ContinueOnError)
	RunFlags(run)

	check := flag.NewFlagSet("check", flag.ContinueOnError)
	CheckFlags(check)

	return []Command{{Name: "run", Flags: run}, {Name: "check", Flags: check}}
}
