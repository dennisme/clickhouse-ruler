package query

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"

	"github.com/dennisme/clickhouse-ruler/internal/lint"
)

// probeTimeout bounds one probe.
//
// A denial arrives immediately, so this only ever bounds a probe that got
// past the privilege check: a granted url() against a refused port retries
// inside ClickHouse for over two minutes before giving up, and the answer was
// already known at the first byte.
const probeTimeout = 3 * time.Second

// codeAccessDenied is ClickHouse's error for a missing privilege.
//
// The privilege is checked before the query executes, which is what makes a
// probe meaningful: a denial arrives even when the endpoint the query names
// is unreachable, so it is the grant being reported and nothing else.
const codeAccessDenied = 497

// Status is what one assertion concluded.
type Status int

const (
	// StatusPass means the contract holds for this assertion.
	StatusPass Status = iota

	// StatusFail means it does not, and the ruler can say why.
	StatusFail

	// StatusInconclusive means nothing was learned, usually because
	// ClickHouse never answered. Reported rather than treated as a pass: a
	// check that reads "could not tell" as "fine" is worse than one that does
	// not run.
	StatusInconclusive
)

func (s Status) String() string {
	switch s {
	case StatusPass:
		return "pass"
	case StatusFail:
		return "fail"
	case StatusInconclusive:
		return "inconclusive"
	default:
		return fmt.Sprintf("status(%d)", int(s))
	}
}

// Assertion is one assertion's result.
type Assertion struct {
	Name   string
	Status Status

	// Detail says what was observed. Empty on a pass.
	Detail string
}

// probes are the queries that are supposed to be refused.
//
// Every one of them reads, and every one names an endpoint that can do
// nothing if the privilege turns out to be granted: a port nothing listens
// on, or a file that does not exist. Nothing here writes. A probe that
// succeeds is a finding, so a probe must never be an operation whose success
// changes anything.
var probes = []struct {
	privilege string
	sql       string
}{
	{"URL", "SELECT 1 FROM url('http://127.0.0.1:1/', 'CSV', 'probe String') LIMIT 1"},
	{"FILE", "SELECT 1 FROM file('ruler-privilege-probe.csv', 'CSV', 'probe String') LIMIT 1"},
	{"REMOTE", "SELECT 1 FROM remote('127.0.0.1:1', system, one) LIMIT 1"},
	{"S3", "SELECT 1 FROM s3('http://127.0.0.1:1/ruler-privilege-probe.csv', 'CSV', 'probe String') LIMIT 1"},
}

// constrained lists the settings the ruler sends and what each needs behind
// it. A limit sent without a constraint is a default a rule can raise in one
// SETTINGS clause, so the value being right says nothing (spec 6.7).
var constrained = map[string]bool{
	// true: a bound is enough, since the ruler sends its own value below it.
	"max_execution_time": true,
	"max_memory_usage":   true,
	"max_result_rows":    true,

	// false: nothing may change these at all. Truncating instead of throwing
	// hands the ruler a partial result that looks complete, and instances
	// that disappear resolve alerts (spec 6.7, 6.9).
	"result_overflow_mode":    false,
	"timeout_overflow_mode":   false,
	"skip_unavailable_shards": false,
}

// settingRow is one row of system.settings, which reports a user's effective
// constraints with no privilege needed to read them.
type settingRow struct {
	Name     string
	Value    string
	Min      *string
	Max      *string
	Readonly uint8
}

// constant reports whether a setting is fixed for this user. ClickHouse
// reports 1 for a setting a constraint made const.
func (r settingRow) constant() bool { return r.Readonly == 1 }

func (r settingRow) bounded() bool { return r.Min != nil || r.Max != nil || r.constant() }

// Privileges checks a source's ClickHouse user against the contract in spec
// 6.7.2, running only the assertions asked for.
//
// It is called once per source, at check time and at startup, never per
// evaluation: grants do not change between two ticks in any way worth four
// refused queries of latency in front of a page (spec 6.7.3).
func (q *Querier) Privileges(ctx context.Context, require []string) []Assertion {
	var out []Assertion

	for _, name := range selected(require) {
		switch name {
		case lint.AssertionSourcesRevoked:
			out = append(out, q.assertSourcesRevoked(ctx))
		case lint.AssertionReadonly:
			out = append(out, q.assertReadonly(ctx))
		case lint.AssertionConstraints:
			out = append(out, q.assertConstraints(ctx))
		case lint.AssertionTableReadable:
			out = append(out, q.assertTableReadable(ctx))
		}
	}
	return out
}

// selected orders and filters the requested assertions. An unknown name is
// dropped rather than guessed at: policy rejects those already, so one
// reaching here means something bypassed it.
func selected(require []string) []string {
	want := make(map[string]bool, len(require))
	for _, name := range require {
		want[name] = true
	}

	var out []string
	for _, name := range lint.Assertions() {
		if want[name] {
			out = append(out, name)
		}
	}
	return out
}

func (q *Querier) assertSourcesRevoked(ctx context.Context) Assertion {
	var granted, unknown []string

	for _, p := range probes {
		status, detail := classifyProbe(q.probe(ctx, p.sql), ctx.Err() == nil)
		switch status {
		case StatusFail:
			granted = append(granted, fmt.Sprintf("%s (%s)", p.privilege, detail))
		case StatusInconclusive:
			unknown = append(unknown, fmt.Sprintf("%s (%s)", p.privilege, detail))
		case StatusPass:
		}
	}

	switch {
	case len(granted) > 0:
		return Assertion{
			Name:   lint.AssertionSourcesRevoked,
			Status: StatusFail,
			Detail: "a rule can read data the row policies never see: " + strings.Join(granted, ", "),
		}
	case len(unknown) > 0:
		return Assertion{
			Name:   lint.AssertionSourcesRevoked,
			Status: StatusInconclusive,
			Detail: strings.Join(unknown, ", "),
		}
	default:
		return Assertion{Name: lint.AssertionSourcesRevoked, Status: StatusPass}
	}
}

// probe runs one probe under its own deadline, so a slow one cannot spend the
// budget the others need.
func (q *Querier) probe(ctx context.Context, sql string) error {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()

	return q.conn.Exec(ctx, sql)
}

// classifyProbe reads a probe's outcome, including the one classifyDenial
// cannot see: a probe still running when its deadline passed.
//
// Still running means the query was executing, and a query only executes
// after the privilege check has let it through, so it is the same finding as
// any other answer that is not a denial. That only holds while the rest of
// the check is still being answered, which is what parentAlive reports: when
// the whole check has run out of time, a timeout says nothing about any
// privilege.
func classifyProbe(err error, parentAlive bool) (Status, string) {
	if errors.Is(err, context.DeadlineExceeded) {
		if !parentAlive {
			return StatusInconclusive, "the check ran out of time before this probe answered"
		}
		return StatusFail, fmt.Sprintf(
			"the privilege is granted, the query was still running after %s", probeTimeout)
	}
	return classifyDenial(err)
}

// classifyDenial reads the outcome of a probe that is supposed to be refused.
//
// ClickHouse checks the privilege before it executes, so a denial is the only
// outcome that proves the grant is absent. Any other answer from the server
// means the query got past the privilege check, whatever it failed on
// afterwards, and that is the finding. Only the server not answering at all
// leaves the question open.
func classifyDenial(err error) (Status, string) {
	if err == nil {
		return StatusFail, "the query ran"
	}

	var ex *clickhouse.Exception
	if errors.As(err, &ex) {
		if ex.Code == codeAccessDenied {
			return StatusPass, ""
		}
		return StatusFail, fmt.Sprintf("the privilege is granted, the query then failed with code %d", ex.Code)
	}
	return StatusInconclusive, "clickhouse did not answer: " + err.Error()
}

func (q *Querier) assertTableReadable(ctx context.Context) Assertion {
	// LIMIT 0 so the check costs nothing and reads no rows. Whether the grant
	// is there is answered by the privilege check, before any of that.
	sql := fmt.Sprintf("SELECT 1 FROM %s.%s LIMIT 0", quoteIdent(q.src.Database), quoteIdent(q.src.Table))

	status, detail := classifyReadable(q.conn.Exec(ctx, sql))
	return Assertion{Name: lint.AssertionTableReadable, Status: status, Detail: detail}
}

// classifyReadable reads the outcome of the one query that has to succeed.
//
// A denial is the under-granted direction, and it is the whole reason this
// assertion exists: without it the first evaluation is where an operator
// finds out, which is at whatever hour the rule first ticks. Any other
// failure is a different problem, so it reports as inconclusive rather than
// claiming the grant is missing.
func classifyReadable(err error) (Status, string) {
	if err == nil {
		return StatusPass, ""
	}

	var ex *clickhouse.Exception
	if errors.As(err, &ex) {
		if ex.Code == codeAccessDenied {
			return StatusFail, "the user cannot read the source's own table, so every evaluation will fail"
		}
		return StatusInconclusive, fmt.Sprintf("reading the source's table failed with code %d: %s", ex.Code, ex.Message)
	}
	return StatusInconclusive, "clickhouse did not answer: " + err.Error()
}

func (q *Querier) assertReadonly(ctx context.Context) Assertion {
	rows, err := q.settingRows(ctx, []string{"readonly"})
	if err != nil {
		return Assertion{Name: lint.AssertionReadonly, Status: StatusInconclusive, Detail: err.Error()}
	}
	return evalReadonly(rows)
}

// evalReadonly wants readonly = 2 and nothing else.
//
// 0 lets a rule write. 1 forbids SET outright, so the ruler cannot send the
// per-query limits at all. 2 is the level that allows SET and nothing more,
// which is why the constraints assertion exists alongside this one: the level
// that lets the ruler set a limit is the level that would let a rule raise it
// (spec 6.7).
func evalReadonly(rows []settingRow) Assertion {
	row, ok := rowNamed(rows, "readonly")
	if !ok {
		return Assertion{
			Name:   lint.AssertionReadonly,
			Status: StatusInconclusive,
			Detail: "system.settings returned no row for readonly",
		}
	}

	switch {
	case row.Value != "2":
		return Assertion{
			Name:   lint.AssertionReadonly,
			Status: StatusFail,
			Detail: fmt.Sprintf("readonly is %s, want 2: 0 lets a rule write, 1 refuses the settings the ruler sends", row.Value),
		}
	case !row.constant():
		return Assertion{
			Name:   lint.AssertionReadonly,
			Status: StatusFail,
			Detail: "readonly is 2 but not const, so a query can lower it",
		}
	default:
		return Assertion{Name: lint.AssertionReadonly, Status: StatusPass}
	}
}

func (q *Querier) assertConstraints(ctx context.Context) Assertion {
	names := make([]string, 0, len(constrained))
	for name := range constrained {
		names = append(names, name)
	}
	sort.Strings(names)

	rows, err := q.settingRows(ctx, names)
	if err != nil {
		return Assertion{Name: lint.AssertionConstraints, Status: StatusInconclusive, Detail: err.Error()}
	}
	return evalConstraints(rows)
}

// evalConstraints wants a constraint behind every limit the ruler sends.
//
// Read rather than probed: probing a constraint would mean sending a query
// designed to exceed a limit, which costs the cluster the thing the limit
// exists to prevent (spec 6.7.2).
func evalConstraints(rows []settingRow) Assertion {
	names := make([]string, 0, len(constrained))
	for name := range constrained {
		names = append(names, name)
	}
	sort.Strings(names)

	var unconstrained []string
	for _, name := range names {
		row, ok := rowNamed(rows, name)
		switch {
		case !ok:
			unconstrained = append(unconstrained, name+" (not reported by system.settings)")
		case constrained[name] && !row.bounded():
			unconstrained = append(unconstrained, name+" (no min, max or const constraint)")
		case !constrained[name] && !row.constant():
			unconstrained = append(unconstrained, name+" (not const)")
		}
	}

	if len(unconstrained) > 0 {
		return Assertion{
			Name:   lint.AssertionConstraints,
			Status: StatusFail,
			Detail: "a rule can raise these in its own SETTINGS clause: " + strings.Join(unconstrained, ", "),
		}
	}
	return Assertion{Name: lint.AssertionConstraints, Status: StatusPass}
}

func rowNamed(rows []settingRow, name string) (settingRow, bool) {
	for _, r := range rows {
		if r.Name == name {
			return r, true
		}
	}
	return settingRow{}, false
}

// settingRows reads a user's own effective settings. No privilege is needed:
// system tables come back filtered to what the user may see, and its own
// settings are always among them.
func (q *Querier) settingRows(ctx context.Context, names []string) ([]settingRow, error) {
	rows, err := q.conn.Query(ctx,
		"SELECT name, toString(value), min, max, readonly FROM system.settings WHERE name IN (?)", names)
	if err != nil {
		return nil, fmt.Errorf("reading system.settings: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []settingRow
	for rows.Next() {
		var r settingRow
		if err := rows.Scan(&r.Name, &r.Value, &r.Min, &r.Max, &r.Readonly); err != nil {
			return nil, fmt.Errorf("reading system.settings: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading system.settings: %w", err)
	}
	return out, nil
}

// quoteIdent backticks an identifier so a database or table name with an
// unusual character reaches ClickHouse as one name.
func quoteIdent(s string) string {
	return "`" + strings.ReplaceAll(s, "`", "``") + "`"
}
