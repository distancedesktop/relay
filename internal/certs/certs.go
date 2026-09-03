// Package certs makes the throwaway ECDSA P-256 certificates this spike needs
// and prints the SHA-256 fingerprint a WebTransport client would pin via
// serverCertificateHashes.
//
// The real Distance agent issues 13-day ECDSA P-256 certs because Chrome caps
// serverCertificateHashes-pinned certs at 14 days (SPIKE.md). We mirror that
// shape so the fake agent is a faithful stand-in, and the relay does the same so
// a Go client can pin it identically.
package certs

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"fmt"
	"math/big"
	"net"
	"time"
)

// Validity matches the agent's 13 days: inside Chrome's 14-day cap for
// serverCertificateHashes-pinned certificates.
const Validity = 13 * 24 * time.Hour

// Generated is a freshly minted self-signed cert plus the fingerprint to pin.
type Generated struct {
	TLS         tls.Certificate
	DER         []byte
	Fingerprint string // lowercase hex SHA-256 of the DER, as Chrome computes it
}

// SelfSigned mints an ECDSA P-256 self-signed certificate valid for the given
// DNS names and IPs. localhost and the loopback addresses are always included so
// a Go client dialing 127.0.0.1 works whether or not it pins.
func SelfSigned(commonName string, hosts ...string) (*Generated, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate P-256 key: %w", err)
	}

	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, fmt.Errorf("generate serial: %w", err)
	}

	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: commonName},
		// Backdate slightly so a client with a marginally slow clock still
		// accepts the cert the instant we print its fingerprint.
		NotBefore:             now.Add(-1 * time.Minute),
		NotAfter:              now.Add(Validity),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		DNSNames:              []string{"localhost"},
		IPAddresses:           []net.IP{net.IPv4(127, 0, 0, 1), net.IPv6loopback},
	}
	for _, h := range hosts {
		if ip := net.ParseIP(h); ip != nil {
			tmpl.IPAddresses = append(tmpl.IPAddresses, ip)
			continue
		}
		if h != "" && h != "localhost" {
			tmpl.DNSNames = append(tmpl.DNSNames, h)
		}
	}

	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, fmt.Errorf("create certificate: %w", err)
	}
	sum := sha256.Sum256(der)
	return &Generated{
		TLS: tls.Certificate{
			Certificate: [][]byte{der},
			PrivateKey:  key,
			Leaf:        tmpl,
		},
		DER:         der,
		Fingerprint: hex.EncodeToString(sum[:]),
	}, nil
}

// FingerprintOf returns the lowercase hex SHA-256 of a DER-encoded certificate.
func FingerprintOf(der []byte) string {
	sum := sha256.Sum256(der)
	return hex.EncodeToString(sum[:])
}

// LoadPair loads a cert/key pair from disk and reports the leaf's fingerprint so
// the relay can print it regardless of where the cert came from.
func LoadPair(certFile, keyFile string) (*Generated, error) {
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, fmt.Errorf("load key pair: %w", err)
	}
	if len(cert.Certificate) == 0 {
		return nil, fmt.Errorf("%s contains no certificate", certFile)
	}
	return &Generated{
		TLS:         cert,
		DER:         cert.Certificate[0],
		Fingerprint: FingerprintOf(cert.Certificate[0]),
	}, nil
}

// PinVerifier builds a tls.Config VerifyPeerCertificate that accepts exactly the
// certificates whose SHA-256 is in fingerprints. This is the Go equivalent of the
// browser's serverCertificateHashes: it deliberately skips CA and hostname
// validation, which is what lets the agent's self-signed cert work at all.
func PinVerifier(fingerprints ...string) func([][]byte, [][]*x509.Certificate) error {
	want := make(map[string]struct{}, len(fingerprints))
	for _, f := range fingerprints {
		want[normalizeHex(f)] = struct{}{}
	}
	return func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
		for _, raw := range rawCerts {
			if _, ok := want[FingerprintOf(raw)]; ok {
				return nil
			}
		}
		got := make([]string, 0, len(rawCerts))
		for _, raw := range rawCerts {
			got = append(got, FingerprintOf(raw))
		}
		return fmt.Errorf("certificate fingerprint mismatch: got %v, want one of %v", got, fingerprints)
	}
}

func normalizeHex(s string) string {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'A' && c <= 'F':
			out = append(out, c+('a'-'A'))
		case (c >= 'a' && c <= 'f') || (c >= '0' && c <= '9'):
			out = append(out, c)
		}
	}
	return string(out)
}
