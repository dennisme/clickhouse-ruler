package source

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/dennisme/clickhouse-ruler/internal/lint"
)

// tlsMinVersion is the floor every connection this ruler makes negotiates
// from. A cluster that cannot do TLS 1.2 is a cluster whose encryption is
// decorative, and the sources file has no field to lower it: an operator who
// needs an older server reachable has a server to fix.
const tlsMinVersion = uint16(tls.VersionTLS12)

// TLS is a source's transport security.
//
// The two halves are held differently, because they rotate differently.
//
// The CA is the bytes that were read, because a reload reuses a connection
// only when the source it was opened with is reflect.DeepEqual to the one just
// read (cmd/ruler/reload.go), and replacing a CA is the one TLS change that
// has to reopen the connection: crypto/tls takes its roots as a built pool and
// offers no callback for them. Bytes compare, so a replaced bundle reopens the
// connection and an unchanged one keeps it. A built tls.Config could not do
// this job: its x509.CertPool holds a closure per certificate and two closures
// are never equal, so every reload would reopen every source that configured
// TLS.
//
// The client pair is the two paths, read at each handshake. A certificate
// manager rotates that pair on its own schedule and expects nothing to be
// signalled, and the pair is the half crypto/tls does offer a callback for.
// Holding the bytes instead would present the old certificate until somebody
// reloaded, and once it expired the source would stop evaluating within the
// hour clickhouse-go keeps a connection for. Keeping the paths also means the
// private key is never held by this process between handshakes.
//
// Which is the split opentelemetry-collector's configtls arrives at from the
// other direction: its reload_interval re-reads the certificate and the key
// behind a GetCertificate callback and leaves the CA alone (configtls.go,
// certReloader). The difference is the timer. The handshake is the moment the
// material is needed, so reading it there needs no interval to be chosen and
// no cache to go stale.
type TLS struct {
	// CA is the PEM bundle the server is verified against. Empty means the
	// host's trust store, and a bundle here replaces it rather than adding to
	// it: a private CA is the only thing that should be able to vouch for the
	// cluster it signed.
	CA []byte

	// CertFile and KeyFile are the client pair ClickHouse authenticates for
	// mTLS, read at the handshake rather than kept.
	CertFile, KeyFile string

	// ServerName is the name verified in the server's certificate. Empty
	// leaves crypto/tls verifying the host in Address, which is the name on
	// the certificate in every case but an IP or a tunnel.
	ServerName string

	InsecureSkipVerify bool
}

// Config is what the driver connects with.
//
// Built per connection rather than kept. The error is what parsing already
// reported as a finding: a source that failed its checks is a source whose
// material is wrong, and this is the second place that would discover it.
func (t *TLS) Config() (*tls.Config, error) {
	cfg := &tls.Config{
		MinVersion: tlsMinVersion,
		ServerName: t.ServerName,
		// The downgrade an operator asked for, reported by File.InsecureTLS,
		// which needs a clock to read the exemption that clears it.
		InsecureSkipVerify: t.InsecureSkipVerify, //nolint:gosec // G402: reported by source/tls-insecure
	}

	if len(t.CA) > 0 {
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(t.CA) {
			return nil, fmt.Errorf("the certificate authority holds no PEM certificate")
		}
		cfg.RootCAs = pool
	}

	if t.CertFile != "" {
		cfg.GetClientCertificate = t.clientCertificate
	}

	return cfg, nil
}

// clientCertificate reads the pair at each handshake, which is what makes a
// rotation on disk reach the cluster with nothing signalled.
//
// Not cached. A handshake happens when the driver opens a connection and when
// it replaces one it has held for its connection lifetime, which is an hour by
// default, so two file reads per handshake cost nothing worth a cache that
// could serve a certificate that has since expired.
func (t *TLS) clientCertificate(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
	pair, err := tls.LoadX509KeyPair(t.CertFile, t.KeyFile)
	if err != nil {
		// Named, because this is reported at a handshake rather than against
		// the line of a file, and the paths are the only thing an operator has
		// to go on. Neither error from crypto/tls echoes key material.
		return nil, fmt.Errorf("reading cert_file %q with key_file %q: %w", t.CertFile, t.KeyFile, err)
	}
	return &pair, nil
}

// tlsRef is what a source said about transport security.
//
// Paths rather than material. resolveTLS reads them, so a certificate that
// cannot be read is a finding with the line of the field that named it, found
// by `ruler check` rather than by a daemon at its first evaluation (spec 6.2).
type tlsRef struct {
	// set is whether a tls_config block was written at all, which is what
	// turns TLS on without `secure`.
	set bool

	caFile             string
	certFile           string
	keyFile            string
	serverName         string
	insecureSkipVerify bool
}

// parseTLSConfig reads a source's tls_config block.
func parseTLSConfig(r *lint.Reader, n *yaml.Node, lines lint.Lines) tlsRef {
	ref := tlsRef{set: true}
	if !r.Mapping(n, "tls_config") {
		return ref
	}

	for _, e := range lint.Entries(n) {
		lines.Set("tls_config."+e.Key.Value, e.Key.Line)
		switch e.Key.Value {
		case "ca_file":
			ref.caFile, _ = r.Scalar(e.Value, "ca_file")
		case "cert_file":
			ref.certFile, _ = r.Scalar(e.Value, "cert_file")
		case "key_file":
			ref.keyFile, _ = r.Scalar(e.Value, "key_file")
		case "server_name":
			ref.serverName, _ = r.Scalar(e.Value, "server_name")
		case "insecure_skip_verify":
			ref.insecureSkipVerify, _ = r.Bool(e.Value, "insecure_skip_verify")
		default:
			r.UnknownField(e.Key, "tls_config")
		}
	}
	return ref
}

// resolveTLS reads the material a source's transport security is built from,
// nil for a plaintext connection.
//
// secure on its own means the host's trust store, which is the whole of the
// managed service case. A tls_config turns TLS on by itself, so `secure: false`
// beside one is a contradiction rather than a precedence rule: one of the two
// is stale, and silently choosing either connects in a way nobody asked for.
func (s Source) resolveTLS(r *lint.Reader, secure bool, ref tlsRef) *TLS {
	line := s.lines.Of("secure", "tls_config")

	switch {
	case ref.set && s.lines.Has("secure") && !secure:
		r.Add(line, lint.CheckSourceTLS, lint.SeverityError,
			"secure: false and tls_config contradict each other, set one")
		return nil
	case !ref.set && !secure:
		return nil
	}

	return tlsReader{r: r, lines: s.lines, check: lint.CheckSourceTLS}.resolve(ref)
}

// tlsReader reads one tls_config's material, reporting under the check name
// and the line map of the block that wrote it.
//
// The reader rather than the block, because a source and an Alertmanager set
// configure the same five fields and fail on them for the same five reasons
// (spec 6.5). What differs is the check a finding carries and which block's
// lines it points at, which is what this holds.
type tlsReader struct {
	r     *lint.Reader
	lines lint.Lines
	check string
}

// resolve reads the material a tls_config names, which is the CA as bytes and
// the client pair as the two paths.
func (t tlsReader) resolve(ref tlsRef) *TLS {
	material := &TLS{
		ServerName:         ref.serverName,
		InsecureSkipVerify: ref.insecureSkipVerify,
	}
	if ref.caFile != "" {
		material.CA = t.readCA(ref.caFile)
	}
	t.readClientCertificate(material, ref)

	return material
}

// readCA reads the PEM bundle the server is verified against.
func (t tlsReader) readCA(path string) []byte {
	line := t.lines.Of("tls_config.ca_file", "tls_config")

	raw, ok := t.readMaterial(line, "ca_file", path)
	if !ok {
		return nil
	}
	// Checked here rather than left to the handshake: a bundle with no
	// certificate in it verifies nothing, and the driver would report that as
	// a failure to connect.
	if !x509.NewCertPool().AppendCertsFromPEM(raw) {
		t.r.Add(line, t.check, lint.SeverityError,
			"ca_file %q contains no PEM certificate", path)
		return nil
	}
	return raw
}

// readClientCertificate reads the pair the server authenticates for mTLS.
func (t tlsReader) readClientCertificate(material *TLS, ref tlsRef) {
	if ref.certFile == "" && ref.keyFile == "" {
		return
	}
	// One without the other is always a mistake: a certificate with no key
	// cannot be presented, and a key with no certificate is not sent at all,
	// which looks like a cluster refusing a credential that was never offered.
	if ref.certFile == "" || ref.keyFile == "" {
		t.r.Add(t.lines.Of("tls_config.cert_file", "tls_config.key_file", "tls_config"),
			t.check, lint.SeverityError, "cert_file and key_file go together, set both")
		return
	}

	line := t.lines.Of("tls_config.cert_file", "tls_config")
	cert, certOK := t.readMaterial(line, "cert_file", ref.certFile)
	key, keyOK := t.readMaterial(
		t.lines.Of("tls_config.key_file", "tls_config"), "key_file", ref.keyFile)
	if !certOK || !keyOK {
		return
	}

	// Parsed here and then discarded: the paths are what the handshake reads,
	// so this is a check rather than a load. It is worth doing anyway, because
	// half a rotation is otherwise a handshake failure at an arbitrary hour
	// rather than a finding in front of whoever changed the file. The reason
	// crypto/tls gives names neither file, so the finding names both: which of
	// the two is stale is the question an operator has.
	if _, err := tls.X509KeyPair(cert, key); err != nil {
		t.r.Add(line, t.check, lint.SeverityError,
			"cert_file %q and key_file %q are not a pair: %s", ref.certFile, ref.keyFile, err)
		return
	}
	material.CertFile, material.KeyFile = ref.certFile, ref.keyFile
}

// readMaterial reads one PEM file, reporting what password_file reports for
// the same two failures and for the same reasons: a read error names the path
// and never the contents, and an empty file is a projected secret volume that
// has not populated yet.
func (t tlsReader) readMaterial(line int, field, path string) ([]byte, bool) {
	raw, err := os.ReadFile(path) //nolint:gosec // an operator-supplied path is the input
	if err != nil {
		t.r.Add(line, t.check, lint.SeverityError,
			"cannot read %s %q: %s", field, path, errReason(err))
		return nil, false
	}
	if len(raw) == 0 {
		t.r.Add(line, t.check, lint.SeverityError, "%s %q is empty", field, path)
		return nil, false
	}
	return raw, true
}

// InsecureTLS reports every source that turned certificate verification off
// without an unexpired exemption for it.
//
// Read with a clock rather than at parse time, for the reason an expired
// exemption is: whether a downgrade is still agreed to is state, not shape. An
// error rather than a warning, because the condition never clears on its own
// and a warning on every run is a warning nobody reads. An exemption is what
// clears it, which costs a reason and a date in the operator's own file
// (spec 6.2, 7.7).
func (f *File) InsecureTLS(now time.Time) []lint.Problem {
	var out []lint.Problem

	for _, s := range f.Sources {
		if s.TLS == nil || !s.TLS.InsecureSkipVerify ||
			s.Exempts(lint.CheckSourceTLSInsecure, now) {
			continue
		}
		p := lint.NewProblem(f.File,
			s.lines.Of("tls_config.insecure_skip_verify", "tls_config"),
			lint.CheckSourceTLSInsecure, lint.SeverityError,
			"insecure_skip_verify turns certificate verification off: "+
				"the connection is encrypted against a server nothing identified")
		p.Subject = s.Name
		out = append(out, p)
	}

	// The same downgrade on the path a page travels, under its own name and
	// its own exemption. It does not refuse a start, which is the one place an
	// alertmanager error does not: the delivery path works and what is missing
	// is the server's identity (spec 6.5).
	for _, a := range f.Alertmanagers {
		if a.TLS == nil || !a.TLS.InsecureSkipVerify ||
			a.Exempts(lint.CheckAlertmanagerTLSInsecure, now) {
			continue
		}
		p := lint.NewProblem(f.File,
			a.lines.Of("tls_config.insecure_skip_verify", "tls_config"),
			lint.CheckAlertmanagerTLSInsecure, lint.SeverityError,
			"insecure_skip_verify turns certificate verification off: "+
				"alerts are posted over an encrypted connection to a server nothing identified")
		p.Subject = a.Subject()
		out = append(out, p)
	}
	return out
}
