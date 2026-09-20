package notify

import (
	"strings"
	"testing"

	"github.com/dennisme/clickhouse-ruler/internal/alert"
)

func TestRenderAnnotations(t *testing.T) {
	a := alert.Alert{
		Labels: map[string]string{"alertname": "HighP99Latency", "ServiceName": "checkout"},
		Value:  1234.5,
	}

	got, err := Render(map[string]string{
		"summary":     "{{ .ServiceName }} p99 is {{ .value }}ms",
		"runbook_url": "https://runbooks.internal/high-p99-latency",
	}, a)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}

	if want := "checkout p99 is 1234.5ms"; got["summary"] != want {
		t.Errorf("summary = %q, want %q", got["summary"], want)
	}
	if want := "https://runbooks.internal/high-p99-latency"; got["runbook_url"] != want {
		t.Errorf("runbook_url = %q, want %q", got["runbook_url"], want)
	}
}

// A summary referencing a label the alert does not carry must fail loudly. The
// alternative is a page that reads "  p99 is ms", which is worse than no page
// because it wastes the responder's first minutes.
func TestRenderRejectsUnknownLabel(t *testing.T) {
	a := alert.Alert{Labels: map[string]string{"alertname": "X"}, Value: 1}

	_, err := Render(map[string]string{"summary": "{{ .NoSuchColumn }} is bad"}, a)
	if err == nil {
		t.Fatal("expected an error for a template referencing a missing label")
	}
	if !strings.Contains(err.Error(), "NoSuchColumn") {
		t.Errorf("error should name the missing label, got: %v", err)
	}
}

// The value is the number the rule alerted on, so it has to be addressable
// alongside the labels rather than only through a label.
func TestRenderFormatsValueWithoutExponent(t *testing.T) {
	a := alert.Alert{Labels: map[string]string{"alertname": "X"}, Value: 1e7}

	got, err := Render(map[string]string{"summary": "{{ .value }}"}, a)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if want := "10000000"; got["summary"] != want {
		t.Errorf("summary = %q, want %q", got["summary"], want)
	}
}
