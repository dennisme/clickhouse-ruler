package source

import (
	"crypto/tls"
	"crypto/x509"
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

// tlsRef is what a source said about transport security.
//
// Paths rather than material. resolve reads them, so a certificate that cannot
// be read is a finding with the line of the field that named it, found by
// `ruler check` rather than by a daemon at its first evaluation (spec 6.2).
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

// resolveTLS builds the connection's TLS configuration, nil for a plaintext
// connection.
//
// secure on its own means the host's trust store, which is the whole of the
// managed service case. A tls_config turns TLS on by itself, so `secure: false`
// beside one is a contradiction rather than a precedence rule: one of the two
// is stale, and silently choosing either connects in a way nobody asked for.
func (s Source) resolveTLS(r *lint.Reader, secure bool, ref tlsRef) *tls.Config {
	line := s.lines.Of("secure", "tls_config")

	switch {
	case ref.set && s.lines.Has("secure") && !secure:
		r.Add(line, lint.CheckSourceTLS, lint.SeverityError,
			"secure: false and tls_config contradict each other, set one")
		return nil
	case !ref.set && !secure:
		return nil
	}

	cfg := &tls.Config{
		MinVersion: tlsMinVersion,
		// Empty leaves crypto/tls verifying the host in Address, which is the
		// name on the certificate in every case but an IP or a tunnel.
		ServerName: ref.serverName,
		// The downgrade an operator asked for. It is reported by
		// File.InsecureTLS, which needs a clock to read the exemption that
		// clears it, so nothing here refuses it.
		InsecureSkipVerify: ref.insecureSkipVerify, //nolint:gosec // G402: reported by source/tls-insecure
	}

	if ref.caFile != "" {
		cfg.RootCAs = s.rootCAs(r, ref.caFile)
	}
	s.clientCertificate(r, cfg, ref)

	return cfg
}

// rootCAs reads the PEM bundle the server is verified against.
func (s Source) rootCAs(r *lint.Reader, path string) *x509.CertPool {
	line := s.lines.Of("tls_config.ca_file", "tls_config")

	// Read errors name the path, as password_file's do.
	raw, err := os.ReadFile(path) //nolint:gosec // an operator-supplied path is the input
	if err != nil {
		r.Add(line, lint.CheckSourceTLS, lint.SeverityError,
			"cannot read ca_file %q: %s", path, errReason(err))
		return nil
	}
	// A projected secret volume that has not populated yet reads as empty, and
	// an empty pool verifies nothing rather than failing visibly.
	if len(raw) == 0 {
		r.Add(line, lint.CheckSourceTLS, lint.SeverityError, "ca_file %q is empty", path)
		return nil
	}

	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(raw) {
		r.Add(line, lint.CheckSourceTLS, lint.SeverityError,
			"ca_file %q contains no PEM certificate", path)
		return nil
	}
	return pool
}

// clientCertificate loads the pair ClickHouse authenticates for mTLS.
func (s Source) clientCertificate(r *lint.Reader, cfg *tls.Config, ref tlsRef) {
	if ref.certFile == "" && ref.keyFile == "" {
		return
	}
	// One without the other is always a mistake: a certificate with no key
	// cannot be presented, and a key with no certificate is not sent at all,
	// which looks like a cluster refusing a credential that was never offered.
	if ref.certFile == "" || ref.keyFile == "" {
		r.Add(s.lines.Of("tls_config.cert_file", "tls_config.key_file", "tls_config"),
			lint.CheckSourceTLS, lint.SeverityError, "cert_file and key_file go together, set both")
		return
	}

	pair, err := tls.LoadX509KeyPair(ref.certFile, ref.keyFile)
	if err != nil {
		// The reason comes from crypto/tls and names neither file, so the
		// finding names both: half a rotation leaves a key that is not the
		// certificate's, and which of the two is stale is the question.
		r.Add(s.lines.Of("tls_config.cert_file", "tls_config"),
			lint.CheckSourceTLS, lint.SeverityError,
			"cannot load cert_file %q with key_file %q: %s", ref.certFile, ref.keyFile, errReason(err))
		return
	}
	cfg.Certificates = []tls.Certificate{pair}
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
		if s.TLS == nil || !s.TLS.InsecureSkipVerify {
			continue
		}
		if s.Exempts(lint.CheckSourceTLSInsecure, now) {
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
	return out
}
