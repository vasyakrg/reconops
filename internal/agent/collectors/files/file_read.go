// Package files implements the file_read collector.
//
// file_read is a deliberately limited capability: only paths matching the
// agent-side allowlist may be read, and only the first N bytes are returned.
// The allowlist is wired in from agent.yaml at registration time (week 5
// will move it into config; week 2 hard-codes a conservative set).
package files

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/vasyakrg/recon/internal/agent/collect"
)

func init() { collect.Register(&fileRead{}) }

type fileRead struct{}

type FileResult struct {
	Path    string `json:"path"`
	SizeB   int64  `json:"size_bytes"`
	Bytes   int    `json:"bytes_returned"`
	SHA256  string `json:"sha256"`
	Content string `json:"content"`
}

// Default allowlist: directories whose contents are universally non-secret
// observability data. Anything else is rejected. Future config knob in
// agent.yaml may extend this; never user-controllable from the hub.
var allowlistDirs = []string{
	"/etc/",
	"/proc/",
	"/sys/",
	"/var/log/",
	"/run/",
}

// denylist regions that are inside allowlist roots but contain secrets.
var denylistPrefixes = []string{
	"/etc/shadow",
	"/etc/gshadow",
	"/etc/sudoers",
	"/etc/ssh/ssh_host_",
	"/etc/ssl/private/",
	"/etc/recon/",
}

func (fileRead) Manifest() collect.Manifest {
	return collect.Manifest{
		Name:        "file_read",
		Version:     "1.0.0",
		Category:    "files",
		Description: "Read a file from a small, hard-coded allowlist (/etc, /proc, /sys, /var/log, /run) with explicit denylist for secrets. Returns size, sha256 of full file, and the first N bytes (default 64 KiB, max 1 MiB).",
		Reads:       []string{"allowlisted file paths only"},
		ParamsSchema: []collect.ParamSpec{
			{Name: "path", Type: "string", Required: true, Description: "absolute path within the allowlist"},
			{Name: "max_bytes", Type: "int", Default: "65536", Description: "max bytes to return (cap 1048576)"},
		},
	}
}

func (fileRead) Run(_ context.Context, p collect.Params) (collect.Result, error) {
	raw := strings.TrimSpace(p["path"])
	if raw == "" {
		return collect.Result{}, fmt.Errorf("path parameter required")
	}
	clean := filepath.Clean(raw)
	if !filepath.IsAbs(clean) {
		return collect.Result{}, fmt.Errorf("path must be absolute")
	}
	if !pathAllowed(clean) {
		return collect.Result{}, fmt.Errorf("path %q not in allowlist", clean)
	}

	maxBytes := 65536
	if s := p["max_bytes"]; s != "" {
		if n, err := strconv.Atoi(s); err == nil && n > 0 && n <= 1024*1024 {
			maxBytes = n
		}
	}

	f, err := os.Open(clean)
	if err != nil {
		return collect.Result{}, fmt.Errorf("open: %w", err)
	}
	defer func() { _ = f.Close() }()

	st, err := f.Stat()
	if err != nil {
		return collect.Result{}, fmt.Errorf("stat: %w", err)
	}

	hasher := sha256.New()
	buf := make([]byte, maxBytes)
	n, err := io.ReadFull(f, buf)
	if err != nil && err != io.ErrUnexpectedEOF && err != io.EOF {
		return collect.Result{}, fmt.Errorf("read: %w", err)
	}
	hasher.Write(buf[:n])
	// Hash the remainder for full sha256.
	if n == maxBytes {
		if _, err := io.Copy(hasher, f); err != nil {
			return collect.Result{}, fmt.Errorf("read tail for hash: %w", err)
		}
	}

	return collect.Result{Data: FileResult{
		Path:    clean,
		SizeB:   st.Size(),
		Bytes:   n,
		SHA256:  hex.EncodeToString(hasher.Sum(nil)),
		Content: string(buf[:n]),
	}}, nil
}

func pathAllowed(p string) bool {
	for _, deny := range denylistPrefixes {
		if strings.HasPrefix(p, deny) {
			return false
		}
	}
	for _, dir := range allowlistDirs {
		if strings.HasPrefix(p, dir) {
			return true
		}
	}
	return false
}
