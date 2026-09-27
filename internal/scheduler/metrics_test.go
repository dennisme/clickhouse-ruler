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
