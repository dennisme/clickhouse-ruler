package query

import (
	"strings"
	"testing"

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
