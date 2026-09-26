//go:build integration

package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeTwoSources writes a sources file both sources of which one selector
// reaches, so a rule really does run against two of them.
//
// They read the same node through different databases, which is what a
// single-node stack can offer and all this needs: a source is a cluster, a user
// and a table, and the rule's unqualified table name resolves in each source's
// own database. otel_dc2.otel_traces holds Duration as Float64 where
// otel.otel_traces holds it as UInt64, both from
// deploy/clickhouse/init/01-schema.sql.
func writeTwoSources(t *testing.T, dir, secondDatabase, secondUser string) string {
	t.Helper()

	addr := os.Getenv("RULER_CLICKHOUSE_ADDR")
	if addr == "" {
		t.Fatal("RULER_CLICKHOUSE_ADDR is not set, run these through `just integration`")
	}

	path := filepath.Join(dir, "sources.yaml")
	body := "sources:\n" +
		"  - name: payments_dc1\n" +
		"    labels: {team: payments, cluster: dc1}\n" +
		"    address: " + addr + "\n" +
		"    database: otel\n" +
		"    username: ruler_payments\n" +
		"    table: otel_traces\n" +
		"    timestamp_column: Timestamp\n" +
		"  - name: payments_dc2\n" +
		"    labels: {team: payments, cluster: dc2}\n" +
		"    address: " + addr + "\n" +
		"    database: " + secondDatabase + "\n" +
		"    username: " + secondUser + "\n" +
		"    table: otel_traces\n" +
		"    timestamp_column: Timestamp\n"

	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("writing sources: %v", err)
	}
	return path
}

// The table name is unqualified on purpose: it resolves in each source's own
// database, which is how one rule reads a different table per cluster.
const estateExpr = `SELECT ServiceName, max(Duration) AS value
FROM otel_traces
WHERE Timestamp >= {{ .From }} AND Timestamp < {{ .To }}
GROUP BY ServiceName`

func checkEstate(t *testing.T, secondDatabase, secondUser string, args ...string) (int, string) {
	t.Helper()

	dir := t.TempDir()
	rules := writeRule(t, dir, estateExpr)
	sources := writeTwoSources(t, dir, secondDatabase, secondUser)

	var stdout, stderr bytes.Buffer
	argv := append([]string{"check", "--sources", sources, "--online"}, args...)
	argv = append(argv, rules)

	code := run(argv, &stdout, &stderr)
	return code, stdout.String() + stderr.String()
}

// The failure this check exists for, against a real server. Both clusters
// resolve the query, so every other tier 1 check passes against both, and the
// rule still produces a value of a different type on each. Nothing but a
// comparison across sources can say so, which is why this is worth a container:
// the unit tests prove the comparison and only this proves the columns reach it
// from two real DESCRIBE answers (spec 6.10).
func TestCheckOnlineReportsSourcesThatDisagreeOnTheSchema(t *testing.T) {
	code, out := checkEstate(t, "otel_dc2", "ruler_dc2")

	// A warning, so the rule still ships: which sources a selector reaches is
	// the operator's to fix, not the author's (spec 7.6).
	if code != exitOK {
		t.Errorf("exit = %d, want %d at the default severity: %s", code, exitOK, out)
	}
	if !strings.Contains(out, "rule/source-schema") {
		t.Errorf("output does not report the disagreement: %s", out)
	}
	// Both clusters and the column, because a finding naming one of them is
	// the confusing answer this replaces.
	for _, want := range []string{"payments_dc1", "payments_dc2", "value", "UInt64", "Float64"} {
		if !strings.Contains(out, want) {
			t.Errorf("output does not name %q: %s", want, out)
		}
	}
}

// One finding for the rule, not one per source. The rule is what cannot be
// correct everywhere; neither cluster is wrong on its own.
func TestCheckOnlineReportsOneSchemaFindingForTheRule(t *testing.T) {
	_, out := checkEstate(t, "otel_dc2", "ruler_dc2")

	if got := strings.Count(out, "rule/source-schema"); got != 1 {
		t.Errorf("rule/source-schema reported %d times, want once for the rule: %s", got, out)
	}
}

// Two sources carrying the same table is the ordinary estate, and a check that
// cannot stay quiet on one is a check nobody keeps enabled.
func TestCheckOnlineSaysNothingWhenSourcesAgree(t *testing.T) {
	code, out := checkEstate(t, "otel", "ruler_payments")

	if code != exitOK {
		t.Errorf("exit = %d, want %d: %s", code, exitOK, out)
	}
	if strings.Contains(out, "rule/source-schema") {
		t.Errorf("output reports a disagreement between agreeing sources: %s", out)
	}
}

// A cluster whose user cannot read the table has not disagreed with anything.
// It is already reported as rule/table-access, and a second finding saying it
// differently would put the author on a path to nowhere: there is no schema to
// compare, only a grant to fix.
func TestCheckOnlineSaysNothingAboutASourceItCouldNotRead(t *testing.T) {
	code, out := checkEstate(t, "otel_dc2", "ruler_payments")

	if code != exitOK {
		t.Errorf("exit = %d, want %d: %s", code, exitOK, out)
	}
	if !strings.Contains(out, "rule/table-access") {
		t.Errorf("output does not report the source it could not read: %s", out)
	}
	if strings.Contains(out, "rule/source-schema") {
		t.Errorf("output reports a disagreement with a source that never answered: %s", out)
	}
}

// An operator who owns both clusters and would rather the rule never ship half
// working raises it, and then the same disagreement blocks (spec 7.6).
func TestCheckOnlineBlocksOnASchemaDisagreementWhenPolicyRaisesIt(t *testing.T) {
	dir := t.TempDir()
	config := filepath.Join(dir, "ruler.yaml")
	body := "checks:\n  rule/source-schema:\n    severity: error\n"
	if err := os.WriteFile(config, []byte(body), 0o600); err != nil {
		t.Fatalf("writing policy: %v", err)
	}

	code, out := checkEstate(t, "otel_dc2", "ruler_dc2", "--config", config)
	if code != exitFinding {
		t.Errorf("exit = %d, want %d: %s", code, exitFinding, out)
	}
	if !strings.Contains(out, "rule/source-schema") {
		t.Errorf("output does not report the disagreement: %s", out)
	}
}
