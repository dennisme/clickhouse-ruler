package query

import (
	"crypto/tls"
	"strings"
	"testing"

	"github.com/dennisme/clickhouse-ruler/internal/source"
)

// Every path that reaches a cluster goes through options, so TLS being wired
// here is TLS wired for evaluation, the privileges check and the re-check pass
// at once (spec 6.2).
func TestOptionsCarryTheSourcesTLS(t *testing.T) {
	opts, err := options(source.Source{
		Name:     "otel_traces",
		Address:  "clickhouse:9440",
		Database: "otel",
		Username: "ruler",
		TLS:      &source.TLS{ServerName: "ch.internal"},
	})
	if err != nil {
		t.Fatalf("building options: %v", err)
	}

	if opts.TLS == nil {
		t.Fatal("the source's TLS did not reach the driver, so the connection is plaintext")
	}
	if opts.TLS.ServerName != "ch.internal" {
		t.Errorf("ServerName = %q, want ch.internal", opts.TLS.ServerName)
	}
	if opts.TLS.MinVersion != tls.VersionTLS12 {
		t.Errorf("MinVersion = %x, want TLS 1.2", opts.TLS.MinVersion)
	}
}

func TestOptionsArePlaintextWhenTheSourceSaysNothing(t *testing.T) {
	opts, err := options(source.Source{
		Name:     "otel_traces",
		Address:  "clickhouse:9000",
		Database: "otel",
		Username: "ruler",
	})
	if err != nil {
		t.Fatalf("building options: %v", err)
	}

	if opts.TLS != nil {
		t.Error("a source with no TLS material got a TLS config")
	}
}

// The material is validated when the sources file is parsed, so a source that
// reaches here with broken material already failed its checks. Open still has
// to report it rather than connecting in plaintext.
func TestOpenRefusesBrokenTLSMaterial(t *testing.T) {
	const password = "hunter2"

	q, err := Open(source.Source{
		Name:     "otel_traces",
		Address:  "clickhouse:9440",
		Database: "otel",
		Username: "ruler",
		Password: password,
		TLS:      &source.TLS{CA: []byte("not a certificate")},
	}, nil)
	if q != nil {
		_ = q.Close()
	}
	if err == nil {
		t.Fatal("a CA that holds no certificate opened a connection")
	}
	if strings.Contains(err.Error(), password) {
		t.Errorf("password leaked into error: %v", err)
	}
}
