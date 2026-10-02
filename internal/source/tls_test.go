package source

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dennisme/clickhouse-ruler/internal/lint"
)

// No integration test against the compose stack, deliberately.
//
// What this change owns is the configuration handed to the driver, and these
// tests pin it exactly: which files were read, what ends up in the
// tls.Config, and which findings a wrong path produces. The handshake itself
// is crypto/tls's, and a compose service listening on 9440 would be testing
// that.
//
// The half that would earn a real test is mTLS as ClickHouse sees it: a user
// authenticated by certificate rather than by password. That needs the server
// configured for it, a CA and a client pair generated at compose-up, and a
// user whose identity is the certificate, which is a change to the stack that
// stands on its own rather than a cert generated on the side of this one. It
// is worth doing when the stack grows a secure port for any other reason.

// writeCertAndKey generates a self-signed certificate and returns the paths to
// its PEM certificate and key. Generated rather than checked in: a fixture
// certificate expires, and a test that fails on a date nobody changed anything
// on is worse than one that costs a key generation.
func writeCertAndKey(t *testing.T, dir string) (certFile, keyFile string) {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generating key: %v", err)
	}
	template := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "ruler-test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		IsCA:         true,
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, &template, &template, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("creating certificate: %v", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("marshalling key: %v", err)
	}

	certFile = filepath.Join(dir, "cert.pem")
	keyFile = filepath.Join(dir, "key.pem")
	write(t, certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
	write(t, keyFile, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}))
	return certFile, keyFile
}

func write(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
}

// parseSourceYAML parses a one-source file written by the test, so a path to a
// generated certificate can be in it.
func parseSourceYAML(t *testing.T, body string) (*File, []lint.Problem) {
	t.Helper()
	yaml := "sources:\n  - name: s\n    address: clickhouse:9440\n" +
		"    database: otel\n    username: ruler\n" +
		"    table: otel_traces\n    timestamp_column: Timestamp\n" + body
	return Parse("sources.yaml", []byte(yaml), testEnv(nil))
}

// The whole of the managed case: one flag, and the server is verified against
// whatever the host trusts.
func TestSecureUsesTheHostTrustStore(t *testing.T) {
	f, problems := parseSourceYAML(t, "    secure: true\n")
	if len(problems) != 0 {
		t.Fatalf("expected no problems, got %v", problems)
	}

	cfg := f.Sources[0].TLS
	if cfg == nil {
		t.Fatal("secure: true built no TLS config, so the connection would be plaintext")
	}
	if cfg.RootCAs != nil {
		t.Error("RootCAs is set, so the host's trust store is not what verifies the server")
	}
	if cfg.ServerName != "" {
		t.Errorf("ServerName = %q, want empty so crypto/tls takes the host in address", cfg.ServerName)
	}
	if cfg.InsecureSkipVerify {
		t.Error("InsecureSkipVerify is on for a source that did not ask for it")
	}
}

func TestNoTLSByDefault(t *testing.T) {
	f, problems := parseSourceYAML(t, "")
	if len(problems) != 0 {
		t.Fatalf("expected no problems, got %v", problems)
	}
	if f.Sources[0].TLS != nil {
		t.Error("a source that said nothing about TLS got a TLS config")
	}
}

func TestTLSConfigImpliesSecure(t *testing.T) {
	dir := t.TempDir()
	certFile, _ := writeCertAndKey(t, dir)

	f, problems := parseSourceYAML(t, fmt.Sprintf(
		"    tls_config:\n      ca_file: %s\n      server_name: ch.internal\n", certFile))
	if len(problems) != 0 {
		t.Fatalf("expected no problems, got %v", problems)
	}

	cfg := f.Sources[0].TLS
	if cfg == nil {
		t.Fatal("tls_config built no TLS config, so the connection would be plaintext")
	}
	if cfg.RootCAs == nil {
		t.Error("ca_file was not read into RootCAs")
	}
	if cfg.ServerName != "ch.internal" {
		t.Errorf("ServerName = %q, want ch.internal", cfg.ServerName)
	}
}

// What the three comments promised: a client certificate and no password at
// all is a legal source.
func TestClientCertificateWithNoPasswordIsLegal(t *testing.T) {
	dir := t.TempDir()
	certFile, keyFile := writeCertAndKey(t, dir)

	f, problems := parseSourceYAML(t, fmt.Sprintf(
		"    tls_config:\n      cert_file: %s\n      key_file: %s\n", certFile, keyFile))
	if len(problems) != 0 {
		t.Fatalf("expected no problems, got %v", problems)
	}

	s := f.Sources[0]
	if s.Password != "" {
		t.Errorf("password = %q, want empty", s.Password)
	}
	if s.TLS == nil {
		t.Fatal("no TLS config, so ClickHouse would never see a client certificate")
	}
	if len(s.TLS.Certificates) != 1 {
		t.Fatalf("got %d client certificates, want 1", len(s.TLS.Certificates))
	}
}

func TestTLSProblems(t *testing.T) {
	dir := t.TempDir()
	certFile, keyFile := writeCertAndKey(t, dir)
	empty := filepath.Join(dir, "empty.pem")
	write(t, empty, nil)
	notPEM := filepath.Join(dir, "notpem.txt")
	write(t, notPEM, []byte("this is not a certificate"))

	for _, tc := range []struct {
		name string
		body string
		line int
		text string
	}{
		{
			name: "missing ca_file",
			body: "    tls_config:\n      ca_file: " + filepath.Join(dir, "nowhere.pem") + "\n",
			line: 9,
			text: fmt.Sprintf("cannot read ca_file %q: no such file or directory",
				filepath.Join(dir, "nowhere.pem")),
		},
		{
			name: "empty ca_file",
			body: "    tls_config:\n      ca_file: " + empty + "\n",
			line: 9,
			text: fmt.Sprintf("ca_file %q is empty", empty),
		},
		{
			name: "ca_file holds no certificate",
			body: "    tls_config:\n      ca_file: " + notPEM + "\n",
			line: 9,
			text: fmt.Sprintf("ca_file %q contains no PEM certificate", notPEM),
		},
		{
			name: "cert without key",
			body: "    tls_config:\n      cert_file: " + certFile + "\n",
			line: 9,
			text: "cert_file and key_file go together, set both",
		},
		{
			name: "key without cert",
			body: "    tls_config:\n      key_file: " + keyFile + "\n",
			line: 9,
			text: "cert_file and key_file go together, set both",
		},
		{
			name: "secure false with a tls_config",
			body: "    secure: false\n    tls_config:\n      ca_file: " + certFile + "\n",
			line: 8,
			text: "secure: false and tls_config contradict each other, set one",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, problems := parseSourceYAML(t, tc.body)
			if len(problems) != 1 {
				t.Fatalf("got %d problems, want 1: %v", len(problems), problems)
			}
			p := problems[0]
			if p.Check != lint.CheckSourceTLS {
				t.Errorf("check = %s, want %s", p.Check, lint.CheckSourceTLS)
			}
			if p.Severity != lint.SeverityError {
				t.Errorf("severity = %s, want error", p.Severity)
			}
			if p.Line != tc.line {
				t.Errorf("line = %d, want %d", p.Line, tc.line)
			}
			if p.Text != tc.text {
				t.Errorf("text = %q, want %q", p.Text, tc.text)
			}
			if p.Subject != "s" {
				t.Errorf("subject = %q, want s", p.Subject)
			}
		})
	}
}

// A key that is not the certificate's is the mistake an operator makes while
// rotating one of the two, and the driver would report it at the first
// handshake instead.
func TestMismatchedClientKeyIsAFinding(t *testing.T) {
	dir := t.TempDir()
	certFile, _ := writeCertAndKey(t, dir)
	other := t.TempDir()
	_, otherKey := writeCertAndKey(t, other)

	_, problems := parseSourceYAML(t, fmt.Sprintf(
		"    tls_config:\n      cert_file: %s\n      key_file: %s\n", certFile, otherKey))
	if len(problems) != 1 {
		t.Fatalf("got %d problems, want 1: %v", len(problems), problems)
	}
	if problems[0].Check != lint.CheckSourceTLS {
		t.Errorf("check = %s, want %s", problems[0].Check, lint.CheckSourceTLS)
	}
	if !strings.Contains(problems[0].Text, "cert_file") {
		t.Errorf("text does not name the field: %q", problems[0].Text)
	}
}

// insecure_skip_verify is reported with a clock rather than at parse time, for
// the reason an expired exemption is: whether the downgrade is still agreed to
// is state, not shape (spec 6.2, 7.7).
func TestInsecureSkipVerifyIsAFinding(t *testing.T) {
	f, problems := parseSourceYAML(t, "    tls_config:\n      insecure_skip_verify: true\n")
	if len(problems) != 0 {
		t.Fatalf("parse reported %v, want the finding from InsecureTLS instead", problems)
	}
	if !f.Sources[0].TLS.InsecureSkipVerify {
		t.Fatal("insecure_skip_verify was not applied to the TLS config")
	}

	now := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	found := f.InsecureTLS(now)
	if len(found) != 1 {
		t.Fatalf("got %d findings, want 1: %v", len(found), found)
	}
	p := found[0]
	if p.Check != lint.CheckSourceTLSInsecure {
		t.Errorf("check = %s, want %s", p.Check, lint.CheckSourceTLSInsecure)
	}
	if p.Severity != lint.SeverityError {
		t.Errorf("severity = %s, want error", p.Severity)
	}
	if p.Line != 9 {
		t.Errorf("line = %d, want 9", p.Line)
	}
	if p.Subject != "s" {
		t.Errorf("subject = %q, want s", p.Subject)
	}
}

func TestInsecureSkipVerifyIsClearedByAnExemption(t *testing.T) {
	f, problems := parseSourceYAML(t, "    tls_config:\n      insecure_skip_verify: true\n"+
		"    exempt:\n      - check: source/tls-insecure\n"+
		"        reason: staging is self-signed until the internal CA lands\n"+
		"        until: 2026-12-01\n")
	if len(problems) != 0 {
		t.Fatalf("expected no problems, got %v", problems)
	}

	now := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	if found := f.InsecureTLS(now); len(found) != 0 {
		t.Errorf("an exempted downgrade still reported: %v", found)
	}

	// The exemption's own expiry is what ends it, so the finding comes back.
	after := time.Date(2026, 12, 2, 0, 0, 0, 0, time.UTC)
	if found := f.InsecureTLS(after); len(found) != 1 {
		t.Errorf("got %d findings after the exemption expired, want 1", len(found))
	}
}

func TestTLSMinimumVersion(t *testing.T) {
	f, _ := parseSourceYAML(t, "    secure: true\n")
	if got := f.Sources[0].TLS.MinVersion; got != tlsMinVersion {
		t.Errorf("MinVersion = %x, want %x", got, tlsMinVersion)
	}
}
