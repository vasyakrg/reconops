// Package auth implements the hub-side PKI: a self-signed root CA, the hub
// server certificate, and signing of agent client certs from CSRs presented
// during enroll. PROJECT.md §2.1 / §9.1–§9.2.
//
// On first start, the hub generates the CA + server cert in caDir. On
// subsequent starts they are loaded from disk. There is no key rotation in
// MVP — losing the CA key requires re-enrolling every agent.
package auth

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"time"
)

const (
	caCertFile     = "ca.pem"
	caKeyFile      = "ca.key"
	serverCertFile = "server.pem"
	serverKeyFile  = "server.key"
)

type Material struct {
	CACert     *x509.Certificate
	CACertPEM  []byte
	caKey      *ecdsa.PrivateKey
	ServerCert []byte // PEM
	ServerKey  []byte // PEM
}

// Bootstrap loads the PKI from caDir, generating it on first run. dnsNames /
// ipAddrs are baked into the server cert's SAN.
func Bootstrap(caDir string, dnsNames []string, ipAddrs []net.IP) (*Material, error) {
	if err := os.MkdirAll(caDir, 0o700); err != nil {
		return nil, fmt.Errorf("mkdir caDir: %w", err)
	}

	mat := &Material{}
	caExists := fileExists(filepath.Join(caDir, caCertFile)) && fileExists(filepath.Join(caDir, caKeyFile))
	if caExists {
		if err := mat.loadCA(caDir); err != nil {
			return nil, fmt.Errorf("load CA: %w", err)
		}
	} else {
		if err := mat.generateCA(caDir); err != nil {
			return nil, fmt.Errorf("generate CA: %w", err)
		}
	}

	// Regenerate the server cert on every start when the SAN list in
	// hub.yaml differs from the cert on disk. Without this, editing
	// dns_names / ip_addrs after first boot is silent — agents keep
	// hitting "x509: certificate is valid for X, not Y" forever. The CA
	// stays put (regenerating would invalidate every enrolled agent's
	// trust); we only re-issue the cheap server leaf.
	srvExists := fileExists(filepath.Join(caDir, serverCertFile)) && fileExists(filepath.Join(caDir, serverKeyFile))
	regen := !srvExists
	if srvExists {
		match, err := serverCertCovers(filepath.Join(caDir, serverCertFile), dnsNames, ipAddrs)
		if err != nil || !match {
			regen = true
		}
	}
	if regen {
		if err := mat.generateServerCert(caDir, dnsNames, ipAddrs); err != nil {
			return nil, fmt.Errorf("generate server cert: %w", err)
		}
	}
	cert, err := os.ReadFile(filepath.Join(caDir, serverCertFile))
	if err != nil {
		return nil, err
	}
	key, err := os.ReadFile(filepath.Join(caDir, serverKeyFile))
	if err != nil {
		return nil, err
	}
	mat.ServerCert, mat.ServerKey = cert, key
	return mat, nil
}

// serverCertCovers reports whether the server cert at path advertises every
// dns_name + ip_addr the operator configured. Used to decide whether to
// re-issue the leaf when hub.yaml SANs change.
func serverCertCovers(path string, dnsNames []string, ipAddrs []net.IP) (bool, error) {
	pemBytes, err := os.ReadFile(path)
	if err != nil {
		return false, err
	}
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return false, fmt.Errorf("server cert: no PEM block")
	}
	c, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return false, err
	}
	want := map[string]struct{}{}
	for _, n := range dnsNames {
		want[n] = struct{}{}
	}
	for _, ip := range ipAddrs {
		want[ip.String()] = struct{}{}
	}
	have := map[string]struct{}{}
	for _, n := range c.DNSNames {
		have[n] = struct{}{}
	}
	for _, ip := range c.IPAddresses {
		have[ip.String()] = struct{}{}
	}
	if len(have) != len(want) {
		return false, nil
	}
	for k := range want {
		if _, ok := have[k]; !ok {
			return false, nil
		}
	}
	// Symmetric: also detect SANs that exist in the cert but were removed
	// from yaml — operator deleting a hostname expects the trust path to
	// shrink (review M4).
	for k := range have {
		if _, ok := want[k]; !ok {
			return false, nil
		}
	}
	return true, nil
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

func (m *Material) generateCA(caDir string) error {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	serial, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "recon-hub-ca", Organization: []string{"Recon"}},
		NotBefore:             time.Now().Add(-time.Hour).UTC(),
		NotAfter:              time.Now().AddDate(10, 0, 0).UTC(),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return err
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return err
	}
	m.CACert = cert
	m.caKey = key
	m.CACertPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})

	if err := writeFile(filepath.Join(caDir, caCertFile), m.CACertPEM, 0o644); err != nil {
		return err
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return err
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	if err := writeFile(filepath.Join(caDir, caKeyFile), keyPEM, 0o600); err != nil {
		return err
	}
	return nil
}

func (m *Material) loadCA(caDir string) error {
	certPEM, err := os.ReadFile(filepath.Join(caDir, caCertFile))
	if err != nil {
		return err
	}
	keyPEM, err := os.ReadFile(filepath.Join(caDir, caKeyFile))
	if err != nil {
		return err
	}
	cb, _ := pem.Decode(certPEM)
	if cb == nil {
		return errors.New("ca.pem: invalid PEM")
	}
	cert, err := x509.ParseCertificate(cb.Bytes)
	if err != nil {
		return err
	}
	kb, _ := pem.Decode(keyPEM)
	if kb == nil {
		return errors.New("ca.key: invalid PEM")
	}
	key, err := x509.ParseECPrivateKey(kb.Bytes)
	if err != nil {
		return err
	}
	m.CACert = cert
	m.CACertPEM = certPEM
	m.caKey = key
	return nil
}

func (m *Material) generateServerCert(caDir string, dnsNames []string, ipAddrs []net.IP) error {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	serial, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "recon-hub", Organization: []string{"Recon"}},
		NotBefore:    time.Now().Add(-time.Hour).UTC(),
		NotAfter:     time.Now().AddDate(2, 0, 0).UTC(),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     dnsNames,
		IPAddresses:  ipAddrs,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, m.CACert, &key.PublicKey, m.caKey)
	if err != nil {
		return err
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return err
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	m.ServerCert, m.ServerKey = certPEM, keyPEM
	if err := writeFile(filepath.Join(caDir, serverCertFile), certPEM, 0o644); err != nil {
		return err
	}
	if err := writeFile(filepath.Join(caDir, serverKeyFile), keyPEM, 0o600); err != nil {
		return err
	}
	return nil
}

// SignCSR validates a PEM-encoded CSR, signs it as a client cert with CN=
// agentID and returns the cert PEM. Caller is responsible for first consuming
// the bootstrap token.
func (m *Material) SignCSR(csrPEM []byte, agentID string, ttl time.Duration) ([]byte, error) {
	block, _ := pem.Decode(csrPEM)
	if block == nil || block.Type != "CERTIFICATE REQUEST" {
		return nil, errors.New("csr: not a PEM CSR")
	}
	csr, err := x509.ParseCertificateRequest(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse csr: %w", err)
	}
	if err := csr.CheckSignature(); err != nil {
		return nil, fmt.Errorf("csr signature: %w", err)
	}
	if err := assertStrongKey(csr.PublicKey); err != nil {
		return nil, err
	}
	serial, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: agentID, Organization: []string{"Recon Agent"}},
		NotBefore:    time.Now().Add(-time.Hour).UTC(),
		NotAfter:     time.Now().Add(ttl).UTC(),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, m.CACert, csr.PublicKey, m.caKey)
	if err != nil {
		return nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), nil
}

// FingerprintFromCert returns a stable identifier for a peer certificate,
// suitable for logging/audit. SHA-256 over the DER body, hex with colons.
func FingerprintFromCert(cert *x509.Certificate) string {
	return fingerprintBytes(cert.Raw)
}

// FingerprintFromPEM parses a single PEM-encoded certificate and returns its
// fingerprint in the same format as FingerprintFromCert. Used at Enroll
// time, after SignCSR, to record the (agent_id → fingerprint) binding.
func FingerprintFromPEM(certPEM []byte) (string, error) {
	block, _ := pem.Decode(certPEM)
	if block == nil {
		return "", errors.New("fingerprint: not a PEM cert")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return "", err
	}
	return FingerprintFromCert(cert), nil
}

func fingerprintBytes(raw []byte) string {
	sum := sha256.Sum256(raw)
	const hex = "0123456789abcdef"
	out := make([]byte, 0, len(sum)*3-1)
	for i, b := range sum {
		if i > 0 {
			out = append(out, ':')
		}
		out = append(out, hex[b>>4], hex[b&0x0f])
	}
	return string(out)
}

// assertStrongKey rejects CSR public keys that fall below the agreed
// strength floor: ECDSA on P-256/P-384/P-521, or RSA ≥ 2048 bits. Anything
// else (RSA-1024, ed25519 not yet allowed in MVP, oddball curves) is
// refused before signing.
func assertStrongKey(pub any) error {
	switch k := pub.(type) {
	case *ecdsa.PublicKey:
		switch k.Curve {
		case elliptic.P256(), elliptic.P384(), elliptic.P521():
			return nil
		}
		return fmt.Errorf("csr: unsupported ECDSA curve %s", k.Curve.Params().Name)
	case *rsa.PublicKey:
		if k.N.BitLen() < 2048 {
			return fmt.Errorf("csr: RSA key too small (%d bits, minimum 2048)", k.N.BitLen())
		}
		return nil
	default:
		return fmt.Errorf("csr: unsupported public key type %T", pub)
	}
}

func writeFile(path string, data []byte, mode os.FileMode) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, mode); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
