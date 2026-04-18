package conn

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"strings"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	reconpb "github.com/vasyakrg/recon/internal/proto"
)

// Enroll runs the bootstrap exchange against the hub: read the bootstrap
// token from disk, generate a fresh keypair + CSR, call Hub/Enroll, persist
// the returned client cert + CA. After this, the agent uses mTLS only.
//
// Idempotent: if cert and key already exist on disk, returns nil immediately.
func Enroll(ctx context.Context, cfg *Config) error {
	if fileExists(cfg.Hub.Cert) && fileExists(cfg.Hub.Key) && fileExists(cfg.Hub.CACert) {
		return nil
	}

	token, err := readToken(cfg.Hub)
	if err != nil {
		return fmt.Errorf("bootstrap token: %w", err)
	}

	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	csr := &x509.CertificateRequest{Subject: pkix.Name{CommonName: cfg.Identity.ID}}
	csrDER, err := x509.CreateCertificateRequest(rand.Reader, csr, priv)
	if err != nil {
		return err
	}
	csrPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: csrDER})

	// First-time TLS: we don't yet have the hub CA. Use InsecureSkipVerify
	// for this one call, then pin the returned CA to a file. This is safe
	// because the bootstrap token itself authenticates the exchange — a MITM
	// without the token cannot complete enroll, and the resulting client
	// cert would not work for Connect either.
	creds := credentials.NewTLS(&tls.Config{
		InsecureSkipVerify: true, //nolint:gosec // bootstrap-only; token-protected
		MinVersion:         tls.VersionTLS12,
	})
	conn, err := grpc.NewClient(cfg.Hub.Endpoint, grpc.WithTransportCredentials(creds))
	if err != nil {
		return fmt.Errorf("dial hub: %w", err)
	}
	defer func() { _ = conn.Close() }()

	client := reconpb.NewHubClient(conn)
	resp, err := client.Enroll(ctx, &reconpb.EnrollRequest{
		BootstrapToken: token,
		AgentId:        cfg.Identity.ID,
		CsrPem:         csrPEM,
	})
	if err != nil {
		return fmt.Errorf("enroll RPC: %w", err)
	}

	if err := writeFile(cfg.Hub.Cert, resp.ClientCertPem, 0o600); err != nil {
		return err
	}
	if err := writeFile(cfg.Hub.CACert, resp.HubCaPem, 0o644); err != nil {
		return err
	}
	keyDER, err := x509.MarshalECPrivateKey(priv)
	if err != nil {
		return err
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	if err := writeFile(cfg.Hub.Key, keyPEM, 0o600); err != nil {
		return err
	}
	return nil
}

func readToken(h HubConfig) (string, error) {
	if h.BootstrapTokenInline != "" {
		return strings.TrimSpace(h.BootstrapTokenInline), nil
	}
	if h.BootstrapToken == "" {
		return "", errors.New("no bootstrap_token (file or inline) set in config")
	}
	body, err := os.ReadFile(h.BootstrapToken)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(body)), nil
}

func fileExists(p string) bool {
	if p == "" {
		return false
	}
	_, err := os.Stat(p)
	return err == nil
}

func writeFile(path string, data []byte, mode os.FileMode) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, mode); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
