//go:build integration

package query

import (
	"context"
	"testing"
	"time"

	"github.com/dennisme/clickhouse-ruler/internal/rule"
)

// The group every query in this test names. Distinct from testGroup so the
// rows this test reads back are its own.
const commentGroup = "rules/payments.yaml:latency"

// queryLogCount asks how many finished queries the cluster recorded for one
// rule, and what group the comment on them named.
//
// It reads through the admin connection: system.query_log is not the source's
// table, and the ruler's own user is granted that one and nothing else, which
// is the contract in spec 6.7.2 working.
func queryLogCount(t *testing.T, address, ruleName string) (count uint64, group string) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	conn := adminConn(t, address)

	// Rows reach system.query_log on a flush interval, so a test reading it
	// without this one asserts on a buffer that has not been written yet.
	if err := conn.Exec(ctx, "SYSTEM FLUSH LOGS"); err != nil {
		t.Fatalf("flushing the query log: %v", err)
	}

	err := conn.QueryRow(ctx, `
		SELECT count(), max(JSONExtractString(log_comment, 'rule_group'))
		FROM system.query_log
		WHERE type = 'QueryFinish'
		  AND event_time >= now() - INTERVAL 1 HOUR
		  AND JSONExtractString(log_comment, 'ruler') = 'clickhouse-ruler'
		  AND JSONExtractString(log_comment, 'rule') = ?`, ruleName).Scan(&count, &group)
	if err != nil {
		t.Fatalf("reading system.query_log: %v", err)
	}
	return count, group
}

// The whole point of spec 8.5: what the ruler sent is findable on the cluster
// by the rule that sent it, rather than only by the source's username, which
// names a source and so cannot separate one rule from another.
//
// All three paths, because a check that read more than expected is the same
// question as an evaluation that did, asked at check time.
func TestEveryPathCarriesTheLogCommentIntoTheQueryLog(t *testing.T) {
	src := testSource(t)
	q := openQuerier(t, src)
	seed(t, q, []span{{at: anchor.Add(-time.Minute), service: "checkout", duration: 1}})

	// Under a minute on purpose. The driver turns a context deadline into
	// max_execution_time, and the source's profile constrains that to 60
	// seconds, so a longer deadline is refused before the query runs.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	expr := selectWithKey("ResourceAttributes", presentKey)

	if _, err := q.Run(ctx, rule.Rule{Alert: "EvaluatedRule", Expr: expr, Window: time.Hour},
		commentGroup, anchor); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if _, err := q.Inspect(ctx, rule.Rule{Alert: "InspectedRule", Expr: expr},
		commentGroup, Checks{Database: src.Database}); err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if _, err := q.Sample(ctx, rule.Rule{Alert: "SampledRule", Expr: expr, Window: time.Hour},
		commentGroup, SampleChecks{}, anchor); err != nil {
		t.Fatalf("Sample: %v", err)
	}

	for _, ruleName := range []string{"EvaluatedRule", "InspectedRule", "SampledRule"} {
		count, group := queryLogCount(t, src.Address, ruleName)
		if count == 0 {
			t.Errorf("no query in system.query_log names %s, so its cost cannot be read back", ruleName)
			continue
		}
		if group != commentGroup {
			t.Errorf("%s: rule_group = %q, want %q", ruleName, group, commentGroup)
		}
	}
}

// A comment carrying the query text would duplicate a column the log already
// has, and one carrying a label value would put data into a column operators
// group by (spec 8.3).
func TestTheLogCommentCarriesNeitherSQLNorData(t *testing.T) {
	src := testSource(t)
	q := openQuerier(t, src)
	seed(t, q, []span{{at: anchor.Add(-time.Minute), service: "checkout", duration: 1}})

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if _, err := q.Run(ctx, rule.Rule{
		Alert:  "PlainCommentRule",
		Expr:   selectWithKey("ResourceAttributes", presentKey),
		Window: time.Hour,
	}, commentGroup, anchor); err != nil {
		t.Fatalf("Run: %v", err)
	}

	conn := adminConn(t, src.Address)
	if err := conn.Exec(ctx, "SYSTEM FLUSH LOGS"); err != nil {
		t.Fatalf("flushing the query log: %v", err)
	}

	var comment string
	err := conn.QueryRow(ctx, `
		SELECT any(log_comment)
		FROM system.query_log
		WHERE type = 'QueryFinish'
		  AND event_time >= now() - INTERVAL 1 HOUR
		  AND JSONExtractString(log_comment, 'rule') = 'PlainCommentRule'`).Scan(&comment)
	if err != nil {
		t.Fatalf("reading system.query_log: %v", err)
	}

	want := `{"ruler":"clickhouse-ruler","rule_group":"rules/payments.yaml:latency","rule":"PlainCommentRule"}`
	if comment != want {
		t.Errorf("log_comment = %q, want %q", comment, want)
	}
}
