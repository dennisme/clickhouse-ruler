//go:build integration

package dashboards

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"
)

// The panel time range a viewer opens the dashboards on decides
// $__rate_interval, and there is nothing in the file to read it from, so this
// is the one value the test names itself (spec 9.8).
const rateInterval = "1m"

// A series that only exists once the ruler ran and Prometheus scraped it.
const scrapedSeries = "clickhouse_ruler_rule_evaluations_total"

type queryResult struct {
	Status    string `json:"status"`
	ErrorType string `json:"errorType"`
	Error     string `json:"error"`
	Data      struct {
		Result []json.RawMessage `json:"result"`
	} `json:"data"`
}

func prometheusURL(t *testing.T) string {
	t.Helper()

	u := os.Getenv("RULER_PROMETHEUS_URL")
	if u == "" {
		t.Fatal("RULER_PROMETHEUS_URL must be set, run `just integration`")
	}
	return strings.TrimSuffix(u, "/")
}

// query sends one expression to Prometheus and returns what it made of it.
func query(t *testing.T, base, expr string) queryResult {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	body := url.Values{"query": {expr}}.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/api/v1/query", strings.NewReader(body))
	if err != nil {
		t.Fatalf("building query request: %v", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("querying Prometheus: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	var got queryResult
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decoding query response (HTTP %d): %v", resp.StatusCode, err)
	}
	return got
}

// The half of spec 8.6 the checked-in gate cannot reach. That one proves a
// panel names a metric something registers, and a name carrying the wrong
// grouping label or an impossible matcher passes it while drawing nothing.
// Only the datasource can say whether the expression is one it will answer.
//
// Answerable, not interesting: an empty result is a pass. Most of the alert
// rules dashboard is empty while nothing is wrong, and a test that demanded
// otherwise would have to make a rule fire to go green (spec 9.8).
func TestEveryPanelExpressionIsAnswerable(t *testing.T) {
	base := prometheusURL(t)

	for _, file := range []string{operations, alertRules} {
		d := load(t, file)
		for _, expr := range d.Expressions() {
			resolved := d.Resolve(expr, rateInterval)
			got := query(t, base, resolved)
			if got.Status != "success" {
				t.Errorf("%s: Prometheus refused a panel query: %s: %s\n  panel:    %s\n  resolved: %s",
					file, got.ErrorType, got.Error, oneLine(expr), oneLine(resolved))
			}
		}
	}
}

// What the test above cannot fail on, because a well-formed query against a
// metric nobody ever exposed is answered with an empty result just as happily.
// The chain the stack exists for is the ruler running, Prometheus scraping it
// and rules evaluating, and this is the assertion that it holds (spec 9.1
// items 5 and 7).
func TestPrometheusHasScrapedTheRuler(t *testing.T) {
	base := prometheusURL(t)

	deadline := time.Now().Add(60 * time.Second)
	for {
		got := query(t, base, scrapedSeries)
		if got.Status != "success" {
			t.Fatalf("Prometheus refused %s: %s: %s", scrapedSeries, got.ErrorType, got.Error)
		}
		if len(got.Data.Result) > 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s has no series after 60s: the ruler is not running, is not being scraped, or evaluated nothing",
				scrapedSeries)
		}
		time.Sleep(time.Second)
	}
}

// oneLine keeps a multi-line panel expression on the failure's own line.
func oneLine(expr string) string {
	return strings.Join(strings.Fields(expr), " ")
}
