package alert

import (
	"strconv"
	"testing"
)

func benchSamples(n int) []Sample {
	out := make([]Sample, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, Sample{
			Labels: map[string]string{"ServiceName": "svc" + strconv.Itoa(i), "env": "prod"},
			Value:  float64(i),
		})
	}
	return out
}

func BenchmarkEvalWithAnnotations(b *testing.B) {
	r := testRule(0, 0)
	r.Annotations = map[string]string{
		"summary":     "{{ .ServiceName }} p99 is {{ .value }}ms",
		"runbook_url": "https://runbooks.internal/high-p99",
	}
	s := New(r, nil, testSource(), testRetention)
	samples := benchSamples(1000)

	for i := 0; i < b.N; i++ {
		if _, _, err := s.Eval(t0, samples); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkEvalWithoutAnnotations(b *testing.B) {
	s := New(testRule(0, 0), nil, testSource(), testRetention)
	samples := benchSamples(1000)

	for i := 0; i < b.N; i++ {
		if _, _, err := s.Eval(t0, samples); err != nil {
			b.Fatal(err)
		}
	}
}
