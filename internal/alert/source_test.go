package alert

import (
	"testing"
	"time"

	"github.com/dennisme/clickhouse-ruler/internal/source"
)

func dc(name, cluster string) source.Source {
	return source.Source{
		Name:   name,
		Labels: map[string]string{"cluster": cluster},
	}
}

// The same rule evaluated against two sources must produce two alerts. Without
// that, the second evaluation overwrites the first and one cluster recovering
// resolves the other's alert while it is still broken (spec 6.10.1).
func TestSourcesProduceDistinctFingerprints(t *testing.T) {
	now := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	r := testRule(0, 0)

	one := New(r, nil, dc("traces_dc1", "dc1")).Eval(now, []Sample{sample("checkout", 1)})
	two := New(r, nil, dc("traces_dc2", "dc2")).Eval(now, []Sample{sample("checkout", 1)})

	if len(one) != 1 || len(two) != 1 {
		t.Fatalf("expected one alert each, got %d and %d", len(one), len(two))
	}
	if one[0].Fingerprint == two[0].Fingerprint {
		t.Fatal("identical fingerprints: one source would resolve the other's alert")
	}
	if one[0].Labels["source"] != "traces_dc1" {
		t.Errorf("source label = %q, want traces_dc1", one[0].Labels["source"])
	}
	if one[0].Labels["cluster"] != "dc1" {
		t.Errorf("cluster label = %q, want dc1 from the source", one[0].Labels["cluster"])
	}
	if two[0].Labels["cluster"] != "dc2" {
		t.Errorf("cluster label = %q, want dc2", two[0].Labels["cluster"])
	}
}

// Each source keeps its own state, so one recovering says nothing about the
// other. This is the failure the fingerprint split exists to prevent.
func TestOneSourceResolvingLeavesTheOtherFiring(t *testing.T) {
	now := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	r := testRule(0, 0)

	dc1 := New(r, nil, dc("traces_dc1", "dc1"))
	dc2 := New(r, nil, dc("traces_dc2", "dc2"))

	dc1.Eval(now, []Sample{sample("checkout", 1)})
	dc2.Eval(now, []Sample{sample("checkout", 1)})

	// dc1 recovers, dc2 still returns the row.
	later := now.Add(time.Minute)
	gone := dc1.Eval(later, nil)
	still := dc2.Eval(later, []Sample{sample("checkout", 1)})

	if len(gone) != 1 || gone[0].Phase != PhaseResolved {
		t.Fatalf("dc1 should have resolved exactly one alert, got %+v", gone)
	}
	if len(still) != 1 || still[0].Phase != PhaseFiring {
		t.Fatalf("dc2 should still be firing, got %+v", still)
	}
}

// A source's labels describe where the query ran, so the query cannot be
// allowed to contradict them (spec 6.3.1 level 4).
func TestSourceLabelsBeatResultColumns(t *testing.T) {
	now := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)

	got := New(testRule(0, 0), nil, dc("traces_dc1", "dc1")).Eval(now, []Sample{{
		Labels: map[string]string{"ServiceName": "checkout", "cluster": "lies", "source": "lies"},
		Value:  1,
	}})

	if len(got) != 1 {
		t.Fatalf("expected one alert, got %d", len(got))
	}
	if got[0].Labels["cluster"] != "dc1" {
		t.Errorf("cluster = %q, want dc1: a result column may not claim a different cluster", got[0].Labels["cluster"])
	}
	if got[0].Labels["source"] != "traces_dc1" {
		t.Errorf("source = %q, want traces_dc1", got[0].Labels["source"])
	}
}

// testSource is the single-source case: a rule matching exactly one source,
// which is what every test predating spec 6.10 assumed.
func testSource() source.Source { return source.Source{Name: "otel_traces"} }
