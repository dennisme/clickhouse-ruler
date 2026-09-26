package query

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/ClickHouse/clickhouse-go/v2"
)

// testGroup is what every test in this package sends as the rule's group.
var testGroup = Attribution{Group: "probe.yaml:probe", Team: "payments"}

// The comment is read back on the cluster, by an operator who has the query
// text in front of them already. What they cannot get from system.query_log
// is which rule sent it, so that is what the comment says (spec 8.5).
func TestLogCommentNamesTheRuleAndItsGroup(t *testing.T) {
	var got map[string]string
	if err := json.Unmarshal([]byte(logComment("rules/payments.yaml:latency", "CheckoutSlow")), &got); err != nil {
		t.Fatalf("the comment is not JSON an operator can extract from: %v", err)
	}

	want := map[string]string{
		"ruler":      "clickhouse-ruler",
		"rule_group": "rules/payments.yaml:latency",
		"rule":       "CheckoutSlow",
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q", k, got[k], v)
		}
	}

	// Nothing else. The query text is already in the log, and a label value
	// is data: putting one here would give a column operators group by
	// unbounded cardinality (spec 8.3).
	for k := range got {
		if _, ok := want[k]; !ok {
			t.Errorf("the comment carries %q, which is not the rule or its group", k)
		}
	}
}

// A rule name is author-supplied text, and the comment is read back with
// JSONExtractString. A quote in a name must not end the string it sits in.
func TestLogCommentEscapesTheNamesItCarries(t *testing.T) {
	var got map[string]string
	if err := json.Unmarshal([]byte(logComment(`g"1`, `Alert"}, "rule_group": "other`)), &got); err != nil {
		t.Fatalf("a quoted name broke the comment: %v", err)
	}
	if got["rule_group"] != `g"1` {
		t.Errorf("rule_group = %q, want %q", got["rule_group"], `g"1`)
	}
}

// The comment rides beside the caps in spec 6.7, so adding it must not
// replace them: WithSettings takes the whole map, and a settings map that
// lost max_execution_time is a rule with no ceiling on it.
func TestWithLogCommentKeepsTheCapsBesideIt(t *testing.T) {
	got := withLogComment(clickhouse.Settings{"max_execution_time": 30}, "f.yaml:g", "R")

	if got["max_execution_time"] != 30 {
		t.Errorf("max_execution_time = %v, want 30", got["max_execution_time"])
	}
	comment, ok := got["log_comment"].(string)
	if !ok || !strings.Contains(comment, `"rule":"R"`) {
		t.Errorf("log_comment = %v, want the rule's comment", got["log_comment"])
	}
}

// Every path sends one, so the caller with no settings of its own still gets
// a map rather than a nil one written into.
func TestWithLogCommentTakesNoSettings(t *testing.T) {
	if got := withLogComment(nil, "f.yaml:g", "R"); got["log_comment"] == nil {
		t.Error("no log_comment on a query that sends no other settings")
	}
}
