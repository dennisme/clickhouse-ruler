package query

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/dennisme/clickhouse-ruler/internal/rule"
	"github.com/dennisme/clickhouse-ruler/internal/source"
)

// An error message is the easiest way for a secret to reach a log, so nothing
// Open returns may contain the password.
func TestOpenDoesNotLeakPassword(t *testing.T) {
	const password = "hunter2"

	for _, addr := range []string{
		"not a host at all",
		"",
		"host:notaport",
	} {
		q, err := Open(source.Source{
			Name:     "otel_traces",
			Address:  addr,
			Database: "otel",
			Username: "ruler",
			Password: password,
		})
		if err != nil && strings.Contains(err.Error(), password) {
			t.Errorf("address %q: password leaked into error: %v", addr, err)
		}
		if q != nil {
			_ = q.Close()
		}
	}
}

// A driver error from a query reaches a log now that the scheduler logs
// evaluation failures, so Run's errors have to be scrubbed the way Open's
// already were. Connection detail an operator needs stays.
func TestQueryErrorKeepsConnectionDetailAndDropsThePassword(t *testing.T) {
	const password = "hunter2"
	q := &Querier{src: source.Source{
		Name:     "otel_traces",
		Address:  "clickhouse-dc1:9000",
		Database: "otel",
		Username: "ruler",
		Password: password,
	}}

	driverErr := errors.New("code: 516, message: ruler: authentication failed " +
		"with password hunter2 against clickhouse-dc1:9000 database otel")

	got := q.queryErr(rule.Rule{Alert: "SlowCheckout"}, driverErr).Error()

	if strings.Contains(got, password) {
		t.Errorf("password leaked into the error: %q", got)
	}
	for _, want := range []string{"SlowCheckout", "clickhouse-dc1:9000", "otel"} {
		if !strings.Contains(got, want) {
			t.Errorf("error %q lost %q, which an operator needs", got, want)
		}
	}
}

// The same property through the real call path, where the driver produces the
// error rather than the test.
func TestRunDoesNotLeakPassword(t *testing.T) {
	const password = "hunter2"

	// Port 1 is not listening, so the query fails on connect.
	q, err := Open(source.Source{
		Name:     "otel_traces",
		Address:  "127.0.0.1:1",
		Database: "otel",
		Username: "ruler",
		Password: password,
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = q.Close() }()

	_, err = q.Run(context.Background(), rule.Rule{
		Alert:  "SlowCheckout",
		Expr:   "SELECT 1 WHERE Timestamp >= {{.From}} AND Timestamp < {{.To}}",
		Window: time.Minute,
	}, testGroup, time.Now())
	if err == nil {
		t.Fatal("want an error querying a port nothing is listening on")
	}
	if strings.Contains(err.Error(), password) {
		t.Errorf("password leaked into the error: %v", err)
	}
}

func TestRedact(t *testing.T) {
	tests := []struct {
		name     string
		text     string
		password string
		want     string
	}{
		{
			name:     "password appears in text",
			text:     "auth failed for ruler with hunter2",
			password: "hunter2",
			want:     "auth failed for ruler with xxxxx",
		},
		{
			name:     "no password set",
			text:     "dial tcp: connection refused",
			password: "",
			want:     "dial tcp: connection refused",
		},
		{
			name:     "password not present in text",
			text:     "dial tcp: connection refused",
			password: "hunter2",
			want:     "dial tcp: connection refused",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := redact(tc.text, tc.password); got != tc.want {
				t.Errorf("redact =\n %q\nwant\n %q", got, tc.want)
			}
		})
	}
}
