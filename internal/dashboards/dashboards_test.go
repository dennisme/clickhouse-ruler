package dashboards

import (
	"regexp"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/dennisme/clickhouse-ruler/internal/scheduler"
)

// The two dashboards, split by audience: whether the ruler is doing its job,
// and whether a team's own rules work (spec 8.6).
const (
	operations = "operations.json"
	alertRules = "alert-rules.json"
)

func load(t *testing.T, file string) Dashboard {
	t.Helper()

	d, err := Load(file)
	if err != nil {
		t.Fatalf("reading %s: %v", file, err)
	}
	return d
}

// registered is every metric name scheduler.NewMetrics puts on the registry,
// read from the registry itself rather than from a list written twice.
func registered(t *testing.T) map[string]bool {
	t.Helper()

	reg := prometheus.NewRegistry()
	scheduler.NewMetrics(reg)

	descs := make(chan *prometheus.Desc)
	go func() {
		reg.Describe(descs)
		close(descs)
	}()

	// Desc keeps its name unexported and prints it as fqName.
	fqName := regexp.MustCompile(`fqName: "([^"]+)"`)

	out := map[string]bool{}
	for d := range descs {
		if m := fqName.FindStringSubmatch(d.String()); m != nil {
			out[m[1]] = true
		}
	}
	return out
}

// The gate spec 8.6 asks for. A panel querying a metric nobody exposes
// renders an empty graph, which looks exactly like a healthy system, so
// renaming a metric has to fail the build rather than quietly empty a panel
// somebody is on call with.
func TestEveryDashboardMetricIsRegistered(t *testing.T) {
	exposed := registered(t)

	for _, file := range []string{operations, alertRules} {
		d := load(t, file)

		exprs := d.Expressions()
		if len(exprs) == 0 {
			t.Errorf("%s has no expressions, so it asserts nothing", file)
		}

		for _, expr := range exprs {
			for _, name := range MetricNames(expr) {
				if !exposed[name] {
					t.Errorf("%s queries %s, which nothing registers: the panel renders empty\n  %s",
						file, name, expr)
				}
			}
		}
	}
}

// Portable, because the UID is local to whoever imports it (spec 8.6).
func TestDashboardsNameADatasourceVariableRatherThanAUID(t *testing.T) {
	for _, file := range []string{operations, alertRules} {
		raw, err := Read(file)
		if err != nil {
			t.Fatalf("reading %s: %v", file, err)
		}

		for _, uid := range DatasourceUIDs(raw) {
			if !strings.HasPrefix(uid, "$") {
				t.Errorf("%s points at datasource %q, which only exists in the Grafana it was exported from",
					file, uid)
			}
		}

		if !load(t, file).HasVariable("datasource") {
			t.Errorf("%s has no datasource variable, so it cannot be imported anywhere else", file)
		}
	}
}

// One repository holds every team's rules, so a dashboard that cannot be
// narrowed to a group shows an operator somebody else's problem (spec 8.6).
func TestDashboardsCarryARuleGroupTemplate(t *testing.T) {
	for _, file := range []string{operations, alertRules} {
		if !load(t, file).HasVariable("rule_group") {
			t.Errorf("%s has no rule_group variable", file)
		}
	}
}

// The order is the message. A dashboard leads on what it is for: missed
// iterations are 8.2's most important signal, and a rule author opens theirs
// to find out which of their own rules are broken.
func TestEachDashboardLeadsOnWhatItIsFor(t *testing.T) {
	tests := []struct {
		file string
		want string
	}{
		{operations, "clickhouse_ruler_rule_group_iterations_missed_total"},
		{alertRules, "clickhouse_ruler_rule_evaluation_failures_total"},
	}

	for _, tc := range tests {
		panels := load(t, tc.file).AllPanels()
		if len(panels) == 0 {
			t.Fatalf("%s has no panels", tc.file)
		}

		var first []string
		for _, target := range panels[0].Targets {
			first = append(first, MetricNames(target.Expr)...)
		}
		if !slicesContains(first, tc.want) {
			t.Errorf("%s leads on %q, querying %v, want it to lead on %s",
				tc.file, panels[0].Title, first, tc.want)
		}
	}
}

func slicesContains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

func TestMetricNamesReadsTheMetricsAndNothingElse(t *testing.T) {
	tests := []struct {
		expr string
		want []string
	}{
		{
			expr: `sum by (rule_group) (rate(clickhouse_ruler_rule_group_iterations_missed_total{rule_group=~"$rule_group"}[$__rate_interval]))`,
			want: []string{"clickhouse_ruler_rule_group_iterations_missed_total"},
		},
		{
			expr: `time() - max(clickhouse_ruler_rule_group_last_evaluation_timestamp_seconds) by (rule_group)`,
			want: []string{"clickhouse_ruler_rule_group_last_evaluation_timestamp_seconds"},
		},
		{
			expr: `histogram_quantile(0.99, sum(rate(clickhouse_ruler_notification_latency_seconds_bucket[5m])) by (le))`,
			want: []string{"clickhouse_ruler_notification_latency_seconds"},
		},
		{
			expr: `clickhouse_ruler_alerts_active{state="firing"} unless on (rule) clickhouse_ruler_rules_unmatched`,
			want: []string{"clickhouse_ruler_alerts_active", "clickhouse_ruler_rules_unmatched"},
		},
	}

	for _, tc := range tests {
		got := MetricNames(tc.expr)
		if strings.Join(got, ",") != strings.Join(tc.want, ",") {
			t.Errorf("MetricNames(%q) = %v, want %v", tc.expr, got, tc.want)
		}
	}
}
