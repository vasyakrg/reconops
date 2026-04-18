package auth

import (
	"crypto/rand"
	"encoding/base64"
)

// GenerateBootstrapToken produces a 32-byte URL-safe random token. The
// caller stores its sha256 hash in the bootstrap_tokens table.
func GenerateBootstrapToken() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b[:]), nil
}
