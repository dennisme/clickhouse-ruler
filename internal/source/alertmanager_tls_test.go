package source

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dennisme/clickhouse-ruler/internal/lint"
)

// The CA is the bytes that were read and the client pair is the two paths,
// which is the split tls.go draws for a source and the same reasons hold here:
// a bundle has to be comparable so a reload can say it changed, and a pair has
// to be read at the handshake so a rotation reaches Alertmanager with nothing
// signalled (spec 6.5, 6.2).
func TestAlertmanagerTLSConfigIsRead(t *testing.T) {
	dir := t.TempDir()
	certFile, keyFile := writeCertAndKey(t, dir)

	f, problems := parseAM(t, fmt.Sprintf(`
alertmanagers:
  - urls: [https://am:9093]
    tls_config:
      ca_file: %s
      cert_file: %s
      key_file: %s
      server_name: alertmanager.internal
`, certFile, certFile, keyFile))
	if len(problems) != 0 {
		t.Fatalf("expected no problems, got %v", problems)
	}

	material := f.Alertmanagers[0].TLS
	if material == nil {
		t.Fatal("tls_config was not read")
	}
	if len(material.CA) == 0 {
		t.Error("the CA bundle was not read")
	}
	if material.CertFile != certFile || material.KeyFile != keyFile {
		t.Errorf("client pair = %q and %q, want the paths the file named", material.CertFile, material.KeyFile)
	}
	if material.ServerName != "alertmanager.internal" {
		t.Errorf("server_name = %q, want alertmanager.internal", material.ServerName)
	}
}

// An https URL with no tls_config is the public CA case, which is what
// `secure: true` is for a source: encrypt this and trust what the host trusts.
func TestAlertmanagerHTTPSNeedsNoTLSConfig(t *testing.T) {
	f, problems := parseAM(t, `
alertmanagers:
  - urls: [https://am:9093]
`)
	if len(problems) != 0 {
		t.Fatalf("expected no problems, got %v", problems)
	}
	if f.Alertmanagers[0].TLS != nil {
		t.Error("a set with no tls_config built TLS material anyway")
	}
}

func TestAlertmanagerTLSProblems(t *testing.T) {
	dir := t.TempDir()
	certFile, keyFile := writeCertAndKey(t, dir)
	other := t.TempDir()
	_, otherKey := writeCertAndKey(t, other)
	empty := filepath.Join(dir, "empty.pem")
	write(t, empty, nil)
	notPEM := filepath.Join(dir, "notpem.txt")
	write(t, notPEM, []byte("this is not a certificate"))
	missing := filepath.Join(dir, "nowhere.pem")

	for _, tc := range []struct {
		name string
		body string
		want string
	}{
		{
			name: "missing ca_file",
			body: "    tls_config:\n      ca_file: " + missing + "\n",
			want: fmt.Sprintf("cannot read ca_file %q: no such file or directory", missing),
		},
		{
			name: "empty ca_file",
			body: "    tls_config:\n      ca_file: " + empty + "\n",
			want: fmt.Sprintf("ca_file %q is empty", empty),
		},
		{
			name: "ca_file holds no certificate",
			body: "    tls_config:\n      ca_file: " + notPEM + "\n",
			want: fmt.Sprintf("ca_file %q contains no PEM certificate", notPEM),
		},
		{
			name: "cert_file without key_file",
			body: "    tls_config:\n      cert_file: " + certFile + "\n",
			want: "cert_file and key_file go together, set both",
		},
		{
			name: "key_file without cert_file",
			body: "    tls_config:\n      key_file: " + keyFile + "\n",
			want: "cert_file and key_file go together, set both",
		},
		{
			name: "the pair does not match",
			body: "    tls_config:\n      cert_file: " + certFile + "\n      key_file: " + otherKey + "\n",
			want: "are not a pair",
		},
		{
			name: "unknown field",
			body: "    tls_config:\n      min_version: TLS13\n",
			want: `unknown field "min_version"`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, problems := parseAM(t, "alertmanagers:\n  - urls: [https://am:9093]\n"+tc.body)
			check := lint.CheckAlertmanagerTLS
			if strings.Contains(tc.want, "unknown field") {
				check = lint.CheckYAMLUnknownField
			}
			if !reported(problems, check, tc.want) {
				t.Errorf("expected %s saying %q, got %v", check, tc.want, problems)
			}
		})
	}
}

// The scheme is what turns TLS on, so material that no URL can reach is
// refused rather than silently ignored: the ruler would connect in plaintext
// while a reviewer reads a file that names a CA (spec 6.5).
func TestAlertmanagerTLSConfigNeedsEveryURLToBeHTTPS(t *testing.T) {
	dir := t.TempDir()
	certFile, _ := writeCertAndKey(t, dir)

	for _, tc := range []struct {
		name string
		urls string
	}{
		{name: "the only url is plaintext", urls: "[http://am:9093]"},
		{name: "one member of the set is plaintext", urls: "[https://am-0:9093, http://am-1:9093]"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, problems := parseAM(t, fmt.Sprintf(
				"alertmanagers:\n  - urls: %s\n    tls_config:\n      ca_file: %s\n", tc.urls, certFile))
			if !reported(problems, lint.CheckAlertmanagerTLS, "tls_config needs every url to be https://") {
				t.Errorf("a tls_config beside a plaintext url was not refused: %v", problems)
			}
		})
	}
}

// Read with a clock rather than at parse time, for the reason the source side
// is: whether a downgrade is still agreed to is state, not shape.
func TestAlertmanagerInsecureSkipVerifyIsAFinding(t *testing.T) {
	f, problems := parseAM(t, `
alertmanagers:
  - urls: [https://am:9093]
    tls_config:
      insecure_skip_verify: true
`)
	if len(problems) != 0 {
		t.Fatalf("parse reported %v, want the finding from InsecureTLS instead", problems)
	}
	if !f.Alertmanagers[0].TLS.InsecureSkipVerify {
		t.Fatal("insecure_skip_verify was not applied to the TLS material")
	}

	now := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	found := f.InsecureTLS(now)
	if len(found) != 1 {
		t.Fatalf("got %d findings, want 1: %v", len(found), found)
	}
	p := found[0]
	if p.Check != lint.CheckAlertmanagerTLSInsecure {
		t.Errorf("check = %s, want %s", p.Check, lint.CheckAlertmanagerTLSInsecure)
	}
	if p.Severity != lint.SeverityError {
		t.Errorf("severity = %s, want error", p.Severity)
	}
	// The line of the field, so an operator is pointed at what they wrote.
	if p.Line != 5 {
		t.Errorf("line = %d, want the insecure_skip_verify line", p.Line)
	}
	if p.Subject != "https://am:9093" {
		t.Errorf("subject = %q, want the first member of the set", p.Subject)
	}
}

func TestAlertmanagerInsecureSkipVerifyIsClearedByAnExemption(t *testing.T) {
	body := `
alertmanagers:
  - urls: [https://am:9093]
    tls_config:
      insecure_skip_verify: true
    exempt:
      - check: alertmanager/tls-insecure
        reason: the alertmanager is self-signed until the internal CA lands
        until: 2026-12-01
`
	f, problems := parseAM(t, body)
	if len(problems) != 0 {
		t.Fatalf("expected no problems, got %v", problems)
	}

	now := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	if found := f.InsecureTLS(now); len(found) != 0 {
		t.Errorf("an exempted downgrade still reported: %v", found)
	}

	// The date is what ends it, and the expiry is its own finding.
	after := time.Date(2026, 12, 2, 0, 0, 0, 0, time.UTC)
	if found := f.InsecureTLS(after); len(found) != 1 {
		t.Errorf("got %d findings after the exemption expired, want 1", len(found))
	}
	expired := f.ExpiredExemptions(after)
	if len(expired) != 1 {
		t.Fatalf("got %d expiry findings, want 1: %v", len(expired), expired)
	}
	if expired[0].Check != lint.CheckSourceExemption {
		t.Errorf("check = %s, want %s", expired[0].Check, lint.CheckSourceExemption)
	}
	if !strings.Contains(expired[0].Text, "alertmanager/tls-insecure") {
		t.Errorf("the expiry finding does not name the check it covered: %q", expired[0].Text)
	}
}

// The material is what the HTTP client is built from, so the set hands back a
// tls.Config the same way a source does.
func TestAlertmanagerTLSBuildsAConfig(t *testing.T) {
	dir := t.TempDir()
	certFile, keyFile := writeCertAndKey(t, dir)

	f, problems := parseAM(t, fmt.Sprintf(`
alertmanagers:
  - urls: [https://am:9093]
    tls_config:
      ca_file: %s
      cert_file: %s
      key_file: %s
`, certFile, certFile, keyFile))
	if len(problems) != 0 {
		t.Fatalf("expected no problems, got %v", problems)
	}

	cfg, err := f.Alertmanagers[0].TLS.Config()
	if err != nil {
		t.Fatalf("building the config: %v", err)
	}
	if cfg.RootCAs == nil {
		t.Error("the CA bundle did not reach the config")
	}
	if cfg.GetClientCertificate == nil {
		t.Error("the client pair is not read at the handshake")
	}
}
