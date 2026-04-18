package auth

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"net"
	"path/filepath"
	"testing"
	"time"
)

func TestBootstrapIdempotent(t *testing.T) {
	dir := t.TempDir()
	mat1, err := Bootstrap(dir, []string{"hub.local"}, []net.IP{net.ParseIP("127.0.0.1")})
	if err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	mat2, err := Bootstrap(dir, []string{"hub.local"}, []net.IP{net.ParseIP("127.0.0.1")})
	if err != nil {
		t.Fatalf("bootstrap2: %v", err)
	}
	if string(mat1.CACertPEM) != string(mat2.CACertPEM) {
		t.Fatal("CA changed across bootstraps")
	}
	if string(mat1.ServerCert) != string(mat2.ServerCert) {
		t.Fatal("server cert changed across bootstraps")
	}
	if !fileExists(filepath.Join(dir, "ca.key")) {
		t.Fatal("ca.key missing")
	}
}

func makeCSR(t *testing.T, cn string) []byte {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.CertificateRequest{Subject: pkix.Name{CommonName: cn}}
	der, err := x509.CreateCertificateRequest(rand.Reader, tmpl, key)
	if err != nil {
		t.Fatalf("csr: %v", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der})
}

func TestSignCSR(t *testing.T) {
	dir := t.TempDir()
	mat, err := Bootstrap(dir, []string{"hub.local"}, nil)
	if err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	csrPEM := makeCSR(t, "agent-1")
	certPEM, err := mat.SignCSR(csrPEM, "agent-1", 90*24*time.Hour)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}

	block, _ := pem.Decode(certPEM)
	if block == nil {
		t.Fatal("invalid cert pem")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("parse cert: %v", err)
	}
	if cert.Subject.CommonName != "agent-1" {
		t.Fatalf("CN=%q", cert.Subject.CommonName)
	}
	pool := x509.NewCertPool()
	pool.AddCert(mat.CACert)
	if _, err := cert.Verify(x509.VerifyOptions{Roots: pool, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}); err != nil {
		t.Fatalf("verify chain: %v", err)
	}
}

func TestSignCSRRejectsGarbage(t *testing.T) {
	dir := t.TempDir()
	mat, _ := Bootstrap(dir, nil, nil)
	if _, err := mat.SignCSR([]byte("not a csr"), "x", time.Hour); err == nil {
		t.Fatal("expected error")
	}
}

func TestGenerateBootstrapToken(t *testing.T) {
	a, err := GenerateBootstrapToken()
	if err != nil {
		t.Fatalf("gen: %v", err)
	}
	b, _ := GenerateBootstrapToken()
	if a == b || len(a) < 32 {
		t.Fatalf("weak tokens: %q %q", a, b)
	}
}
