package query

import (
	"crypto/tls"
	"testing"

	"github.com/dennisme/clickhouse-ruler/internal/source"
)

// Every path that reaches a cluster goes through options, so TLS being wired
// here is TLS wired for evaluation, the privileges check and the re-check pass
// at once (spec 6.2).
func TestOptionsCarryTheSourcesTLS(t *testing.T) {
	cfg := &tls.Config{MinVersion: tls.VersionTLS12, ServerName: "ch.internal"}

	opts := options(source.Source{
		Name:     "otel_traces",
		Address:  "clickhouse:9440",
		Database: "otel",
		Username: "ruler",
		TLS:      cfg,
	})

	if opts.TLS != cfg {
		t.Fatal("the source's TLS config did not reach the driver, so the connection is plaintext")
	}
}

func TestOptionsArePlaintextWhenTheSourceSaysNothing(t *testing.T) {
	opts := options(source.Source{
		Name:     "otel_traces",
		Address:  "clickhouse:9000",
		Database: "otel",
		Username: "ruler",
	})

	if opts.TLS != nil {
		t.Error("a source with no TLS config got one")
	}
}
