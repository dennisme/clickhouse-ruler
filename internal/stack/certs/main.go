// Command certs writes the private CA and the server pair the stack's TLS
// Alertmanager listens with.
//
// Generated rather than checked in, which is the reason internal/source's TLS
// tests generate theirs: a fixture certificate expires, and a stack that stops
// coming up on a date nobody changed anything on costs more than a key
// generation on every compose-up. A checked-in private key is also a private
// key in a git history, test-only or not.
//
// Go rather than openssl, so the stack needs nothing on the host that `go
// test` does not already need. The names are what the stack reaches the server
// by: the compose service name from inside the network, and localhost with
// 127.0.0.1 from the integration tests on the host.
package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"flag"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"time"
)

// validity is long enough that nobody rotates these by hand and short enough
// that a leaked test key stops working.
const validity = 365 * 24 * time.Hour

func main() {
	out := flag.String("out", "deploy/alertmanager/tls", "directory the material is written to")
	flag.Parse()

	if err := write(*out); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func write(dir string) error {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return err
	}

	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return fmt.Errorf("generating the CA key: %w", err)
	}
	ca := &x509.Certificate{
		SerialNumber:          serial(),
		Subject:               pkix.Name{CommonName: "clickhouse-ruler stack CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(validity),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, ca, ca, &caKey.PublicKey, caKey)
	if err != nil {
		return fmt.Errorf("signing the CA: %w", err)
	}
	signed, err := x509.ParseCertificate(caDER)
	if err != nil {
		return err
	}

	serverKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return fmt.Errorf("generating the server key: %w", err)
	}
	server := &x509.Certificate{
		SerialNumber: serial(),
		Subject:      pkix.Name{CommonName: "alertmanager-tls"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(validity),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		// What the server is reached by, and nothing else: a certificate that
		// carried every name would make the wrong-server_name case unprovable.
		DNSNames:    []string{"alertmanager-tls", "localhost"},
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
	}
	serverDER, err := x509.CreateCertificate(rand.Reader, server, signed, &serverKey.PublicKey, caKey)
	if err != nil {
		return fmt.Errorf("signing the server certificate: %w", err)
	}

	files := []struct {
		name  string
		block *pem.Block
		mode  os.FileMode
	}{
		{"ca.pem", &pem.Block{Type: "CERTIFICATE", Bytes: caDER}, 0o644},
		{"server.pem", &pem.Block{Type: "CERTIFICATE", Bytes: serverDER}, 0o644},
		{"server-key.pem", keyBlock(serverKey), 0o644},
	}
	for _, f := range files {
		if f.block == nil {
			return fmt.Errorf("marshalling %s", f.name)
		}
		// World readable, including the key. The Alertmanager container runs as
		// its own user and reads these through a read-only bind mount, so a
		// tighter mode is a server that will not start, and the material is
		// regenerated on every compose-up.
		if err := os.WriteFile(filepath.Join(dir, f.name), pem.EncodeToMemory(f.block), f.mode); err != nil { //nolint:gosec // G306: read by another container's user
			return fmt.Errorf("writing %s: %w", f.name, err)
		}
	}

	// The CA key is deliberately not written. Nothing signs anything else with
	// it, and a key nobody keeps is a key nobody leaks.
	return nil
}

func keyBlock(key *ecdsa.PrivateKey) *pem.Block {
	der, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil
	}
	return &pem.Block{Type: "EC PRIVATE KEY", Bytes: der}
}

func serial() *big.Int {
	n, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return big.NewInt(time.Now().UnixNano())
	}
	return n
}
