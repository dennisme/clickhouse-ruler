package source

import (
	"errors"
	"os"
	"strings"
	"time"

	"github.com/dennisme/clickhouse-ruler/internal/lint"
	"github.com/dennisme/clickhouse-ruler/internal/policy"
	"gopkg.in/yaml.v3"
)

const (
	checkName            = "source/name"
	checkAddress         = "source/address"
	checkDatabase        = "source/database"
	checkUsername        = "source/username"
	checkPassword        = "source/password"
	checkTable           = "source/table"
	checkTimestampColumn = "source/timestamp-column"
	checkEvaluationDelay = "source/evaluation-delay"
	checkMaxRows         = "source/max-rows"
	checkMaxExecution    = "source/max-execution-time"
	checkMaxMemory       = "source/max-memory-usage"
)

// DefaultEvaluationDelay keeps an evaluation off the newest, still-filling
// window. See spec 6.8.
const DefaultEvaluationDelay = time.Minute

// DefaultMaxRows caps how many alert instances one evaluation may produce, so
// that a query returning millions of rows fails instead of exhausting memory.
const DefaultMaxRows = 1000

// Server side caps sent with every query. The client side row cap protects
// the ruler; these protect the cluster, and a rule that trips one fails on
// its own rather than degrading everything else (spec 6.7).
const (
	DefaultMaxExecutionTime = 30 * time.Second
	DefaultMaxMemoryUsage   = 1 << 30
)

// File is one parsed sources file.
//
// Sources are deliberately not part of a rule file. Credentials, the
// evaluation delay and the cost caps are operator concerns, and keeping them
// in a separate file lets CODEOWNERS stop rule authors from editing them
// (spec 6.6).
type File struct {
	File    string
	Sources []Source
}

// Source is one queryable table and how to reach it.
//
// Everything except the password is written in the file. The username in
// particular is not a secret and must be reviewable: it is the tenancy
// boundary in spec 6.6, where each team's rules run as their own ClickHouse
// user and row policies do the isolation. A username hidden in an environment
// variable cannot be checked in a pull request.
type Source struct {
	Name     string
	Address  string
	Database string
	Username string

	// Password is the resolved secret. It is never written in the file and
	// never included in String.
	Password string

	Table           string
	TimestampColumn string
	EvaluationDelay time.Duration
	MaxRows         int

	// MaxExecutionTime and MaxMemoryUsage are sent as ClickHouse query
	// settings so the cluster enforces them, not the ruler.
	MaxExecutionTime time.Duration
	MaxMemoryUsage   int

	// Policy tightens checks for rules that read this source. A source that
	// pages on-call can demand a runbook without every rule in the repository
	// having to (spec 7.7). It can only tighten: the merge takes the
	// strictest setting across scopes.
	Policy *policy.Policy

	lines lint.Lines
}

// String redacts the password so that printing a Source with %v or %s cannot
// leak it into a log.
func (s Source) String() string {
	password := "unset"
	if s.Password != "" {
		password = "xxxxx"
	}
	return "source " + s.Name + " (" + s.Username + "@" + s.Address +
		"/" + s.Database + ", password " + password + ")"
}

func (f *File) ByName(name string) (Source, bool) {
	for _, s := range f.Sources {
		if s.Name == name {
			return s, true
		}
	}
	return Source{}, false
}

// Parse decodes a sources file, collecting every problem rather than stopping
// at the first. env looks up environment variables and defaults to the real
// environment when nil.
func Parse(file string, data []byte, env func(string) (string, bool)) (*File, []lint.Problem) {
	if env == nil {
		env = os.LookupEnv
	}

	f := &File{File: file}
	r := lint.NewReader(file)

	doc, ok := r.Document(data)
	if !ok {
		return f, r.Problems()
	}
	if !r.Mapping(doc, "file") {
		return f, r.Problems()
	}

	for _, e := range lint.Entries(doc) {
		switch e.Key.Value {
		case "sources":
			f.Sources = parseSources(r, e.Value, env)
		default:
			r.UnknownField(e.Key, "file")
		}
	}

	return f, r.Problems()
}

func parseSources(r *lint.Reader, n *yaml.Node, env func(string) (string, bool)) []Source {
	if !r.Sequence(n, "sources") {
		return nil
	}

	// Source names have to be unique across the file, so the set spans the
	// whole sequence.
	namedAt := map[string]int{}

	sources := make([]Source, 0, len(n.Content))
	for _, item := range n.Content {
		if !r.Mapping(item, "source") {
			continue
		}
		sources = append(sources, parseSource(r, item, env, namedAt))
	}
	return sources
}

// secretRef is where a source says its password comes from. Both forms are
// read so that setting both can be reported rather than silently resolved.
type secretRef struct {
	file string
	env  string
}

func parseSource(r *lint.Reader, n *yaml.Node, env func(string) (string, bool), namedAt map[string]int) Source {
	s := Source{lines: lint.NewLines(n.Line)}
	firstProblem := r.Count()
	var secret secretRef

	for _, e := range lint.Entries(n) {
		s.lines.Set(e.Key.Value, e.Key.Line)
		switch e.Key.Value {
		case "name":
			s.Name, _ = r.Scalar(e.Value, "source name")
		case "address":
			s.Address, _ = r.Scalar(e.Value, "address")
		case "database":
			s.Database, _ = r.Scalar(e.Value, "database")
		case "username":
			s.Username, _ = r.Scalar(e.Value, "username")
		case "password_file":
			secret.file, _ = r.Scalar(e.Value, "password_file")
		case "password_env":
			secret.env, _ = r.Scalar(e.Value, "password_env")
		case "table":
			s.Table, _ = r.Scalar(e.Value, "table")
		case "timestamp_column":
			s.TimestampColumn, _ = r.Scalar(e.Value, "timestamp_column")
		case "evaluation_delay":
			s.EvaluationDelay, _ = r.Duration(e.Value, "evaluation_delay")
		case "max_rows":
			s.MaxRows, _ = r.Int(e.Value, "max_rows")
		case "max_execution_time":
			s.MaxExecutionTime, _ = r.Duration(e.Value, "max_execution_time")
		case "max_memory_usage":
			s.MaxMemoryUsage, _ = r.Int(e.Value, "max_memory_usage")
		case "checks":
			s.Policy = policy.ParseNode(r, e.Value)
		default:
			r.UnknownField(e.Key, "source")
		}
	}

	// Defaults are applied before the checks so that an unset field is never
	// reported for being zero.
	if !s.lines.Has("evaluation_delay") {
		s.EvaluationDelay = DefaultEvaluationDelay
	}
	if !s.lines.Has("max_rows") {
		s.MaxRows = DefaultMaxRows
	}
	if !s.lines.Has("max_execution_time") {
		s.MaxExecutionTime = DefaultMaxExecutionTime
	}
	if !s.lines.Has("max_memory_usage") {
		s.MaxMemoryUsage = DefaultMaxMemoryUsage
	}

	s.checkName(r, namedAt)
	s.checkConnection(r)
	s.Password = s.resolvePassword(r, secret, env)
	s.checkQueryTarget(r)

	r.AttributeFrom(firstProblem, s.Name)
	return s
}

func (s Source) checkName(r *lint.Reader, namedAt map[string]int) {
	line := s.lines.Of("name")

	if s.Name == "" {
		r.Add(line, checkName, lint.SeverityError, "source name is empty")
		return
	}
	if first, ok := namedAt[s.Name]; ok {
		r.Add(line, checkName, lint.SeverityError,
			"duplicate source name %q, first defined on line %d", s.Name, first)
		return
	}
	namedAt[s.Name] = line
}

func (s Source) checkConnection(r *lint.Reader) {
	if s.Address == "" {
		r.Add(s.lines.Of("address"), checkAddress, lint.SeverityError,
			"address is empty, expected host:port")
	}
	if s.Database == "" {
		r.Add(s.lines.Of("database"), checkDatabase, lint.SeverityError, "database is empty")
	}
	// Required rather than defaulted to "default": which ClickHouse user a
	// source connects as is the tenancy boundary, so it has to be a decision
	// somebody wrote down and somebody else reviewed.
	if s.Username == "" {
		r.Add(s.lines.Of("username"), checkUsername, lint.SeverityError, "username is empty")
	}
}

// resolvePassword reads the secret from a file or the environment.
//
// Setting both is an error rather than a precedence rule. If the two differ
// one of them is stale, and silently picking either can mean authenticating
// with a credential that was supposed to have been rotated away.
func (s Source) resolvePassword(r *lint.Reader, secret secretRef, env func(string) (string, bool)) string {
	line := s.lines.Of("password_file", "password_env")

	switch {
	case secret.file != "" && secret.env != "":
		r.Add(line, checkPassword, lint.SeverityError,
			"password_file and password_env are mutually exclusive, set one")
		return ""

	case secret.file != "":
		// Read errors name the path, never the contents.
		raw, err := os.ReadFile(secret.file)
		if err != nil {
			r.Add(line, checkPassword, lint.SeverityError,
				"cannot read password_file %q: %s", secret.file, errReason(err))
			return ""
		}
		// A projected secret volume that has not populated yet reads as
		// empty. Sending that to ClickHouse as a real password produces a
		// confusing auth failure instead of a clear config error.
		password := strings.TrimRight(string(raw), "\r\n")
		if password == "" {
			r.Add(line, checkPassword, lint.SeverityError,
				"password_file %q is empty", secret.file)
			return ""
		}
		return password

	case secret.env != "":
		password, ok := env(secret.env)
		if !ok {
			r.Add(line, checkPassword, lint.SeverityError,
				"password_env references ${%s}, which is not set in the environment", secret.env)
			return ""
		}
		if password == "" {
			r.Add(line, checkPassword, lint.SeverityError,
				"password_env ${%s} is set but empty", secret.env)
			return ""
		}
		return password
	}

	// No password at all is legal: local development against the compose
	// stack, and mTLS where ClickHouse authenticates the client certificate.
	return ""
}

// errReason strips the path the os package prepends, because the caller
// already names the file and the duplication reads badly.
func errReason(err error) string {
	var pathErr *os.PathError
	if errors.As(err, &pathErr) {
		return pathErr.Err.Error()
	}
	return err.Error()
}

func (s Source) checkQueryTarget(r *lint.Reader) {
	if s.Table == "" {
		r.Add(s.lines.Of("table"), checkTable, lint.SeverityError, "table is empty")
	}
	if s.TimestampColumn == "" {
		r.Add(s.lines.Of("timestamp_column"), checkTimestampColumn,
			lint.SeverityError, "timestamp_column is empty")
	}
	if s.EvaluationDelay < 0 {
		r.Add(s.lines.Of("evaluation_delay"), checkEvaluationDelay,
			lint.SeverityError, "evaluation_delay must not be negative, got %s", s.EvaluationDelay)
	}
	if s.MaxRows < 0 {
		r.Add(s.lines.Of("max_rows"), checkMaxRows,
			lint.SeverityError, "max_rows must not be negative, got %d", s.MaxRows)
	}
	if s.MaxExecutionTime < 0 {
		r.Add(s.lines.Of("max_execution_time"), checkMaxExecution,
			lint.SeverityError, "max_execution_time must not be negative, got %s", s.MaxExecutionTime)
	}
	if s.MaxMemoryUsage < 0 {
		r.Add(s.lines.Of("max_memory_usage"), checkMaxMemory,
			lint.SeverityError, "max_memory_usage must not be negative, got %d", s.MaxMemoryUsage)
	}
}
