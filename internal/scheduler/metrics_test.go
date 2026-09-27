package scheduler

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/dennisme/clickhouse-ruler/internal/buildinfo"
)

// The gauge exists so an operator can tell which version each replica runs
// without reaching the binary. Its value carries nothing; the labels are the
// whole point, and they have to match what `ruler version` would say.
func TestBuildInfoCarriesTheBuildAsLabels(t *testing.T) {
	reg := prometheus.NewRegistry()
	NewMetrics(reg)

	families, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}

	var found *prometheus.Labels
	for _, f := range families {
		if f.GetName() != "clickhouse_ruler_build_info" {
			continue
		}
		if got := len(f.GetMetric()); got != 1 {
			t.Fatalf("series = %d, want exactly one per process", got)
		}
		m := f.GetMetric()[0]
		if got := m.GetGauge().GetValue(); got != 1 {
			t.Errorf("value = %v, want 1", got)
		}
		labels := prometheus.Labels{}
		for _, p := range m.GetLabel() {
			labels[p.GetName()] = p.GetValue()
		}
		found = &labels
	}

	if found == nil {
		t.Fatal("clickhouse_ruler_build_info was not registered")
	}

	build := buildinfo.Get()
	want := prometheus.Labels{
		"version":   build.Version,
		"revision":  build.Commit,
		"goversion": build.GoVersion,
	}
	for name, value := range want {
		if (*found)[name] != value {
			t.Errorf("label %s = %q, want %q", name, (*found)[name], value)
		}
	}
	if len(*found) != len(want) {
		t.Errorf("labels = %v, want exactly %v", *found, want)
	}
}

// A rebuild of the metrics, which a reload does not do but a second ruler in
// one test binary does, must not multiply the series.
func TestBuildInfoStaysOneSeries(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewMetrics(reg)

	if got := testutil.CollectAndCount(m.BuildInfo); got != 1 {
		t.Errorf("series = %d, want 1", got)
	}
}

// The question the cost labels exist for is which cluster is slow (spec 8.8),
// and a rule spanning two clusters can only answer it if each source gets its
// own series rather than both folding into one.
func TestQueryDurationKeepsOneSeriesPerSource(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewMetrics(reg)

	m.QueryDuration.WithLabelValues("HighLatency", "api", "payments", "prod_eu").Observe(1)
	m.QueryDuration.WithLabelValues("HighLatency", "api", "payments", "prod_us").Observe(2)

	if got := testutil.CollectAndCount(m.QueryDuration); got != 2 {
		t.Errorf("series = %d, want one per source", got)
	}
	if got := labelValues(t, reg, "clickhouse_ruler_query_duration_seconds", "source"); len(got) != 2 {
		t.Errorf("source values = %v, want two", got)
	}
}

// A source that left the configuration takes its cost series with it, or they
// read as a cluster this ruler still bills for and still measures (spec 8.8).
func TestDeleteSourceClearsCostSeries(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewMetrics(reg)

	for _, src := range []string{"prod_eu", "prod_us"} {
		m.QueryReadRowsTotal.WithLabelValues("HighLatency", "payments", src).Add(1)
		m.QueryReadBytesTotal.WithLabelValues("HighLatency", "payments", src).Add(1)
		m.QueryDuration.WithLabelValues("HighLatency", "api", "payments", src).Observe(1)
		m.QueryQueueWait.WithLabelValues(src).Observe(1)
	}

	m.deleteSource("prod_eu")

	for _, c := range []prometheus.Collector{
		m.QueryReadRowsTotal, m.QueryReadBytesTotal, m.QueryDuration, m.QueryQueueWait,
	} {
		if got := testutil.CollectAndCount(c); got != 1 {
			t.Errorf("series = %d, want only the source still configured", got)
		}
	}
	if got := labelValues(t, reg, "clickhouse_ruler_query_duration_seconds", "source"); len(got) != 1 || !got["prod_us"] {
		t.Errorf("source values = %v, want prod_us alone", got)
	}
}

// Deleting a rule's cost series still has to reach every source it evaluated
// against, which is what the partial match on `rule` alone buys (spec 8.2).
func TestDeleteRuleNameClearsCostSeriesAcrossSources(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewMetrics(reg)

	for _, src := range []string{"prod_eu", "prod_us"} {
		m.QueryReadRowsTotal.WithLabelValues("HighLatency", "payments", src).Add(1)
		m.QueryReadBytesTotal.WithLabelValues("HighLatency", "payments", src).Add(1)
		m.QueryDuration.WithLabelValues("HighLatency", "api", "payments", src).Observe(1)
	}
	m.QueryReadRowsTotal.WithLabelValues("SlowWrites", "storage", "prod_eu").Add(1)
	m.QueryDuration.WithLabelValues("SlowWrites", "storage-group", "storage", "prod_eu").Observe(1)
	m.QueryMemoryUsage.WithLabelValues("HighLatency").Observe(1 << 20)

	m.deleteRuleName("HighLatency")

	if got := testutil.CollectAndCount(m.QueryReadRowsTotal); got != 1 {
		t.Errorf("rows series = %d, want only the other rule", got)
	}
	if got := testutil.CollectAndCount(m.QueryReadBytesTotal); got != 0 {
		t.Errorf("bytes series = %d, want none", got)
	}
	if got := testutil.CollectAndCount(m.QueryDuration); got != 1 {
		t.Errorf("duration series = %d, want only the other rule", got)
	}
	if got := testutil.CollectAndCount(m.QueryMemoryUsage); got != 0 {
		t.Errorf("memory series = %d, want none", got)
	}
}

// labelValues is the set of values one label takes across a metric family.
func labelValues(t *testing.T, reg *prometheus.Registry, metric, label string) map[string]bool {
	t.Helper()

	families, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	values := map[string]bool{}
	for _, f := range families {
		if f.GetName() != metric {
			continue
		}
		for _, m := range f.GetMetric() {
			for _, p := range m.GetLabel() {
				if p.GetName() == label {
					values[p.GetValue()] = true
				}
			}
		}
	}
	return values
}
