// Package files implements the file_read collector.
//
// file_read is a deliberately limited capability: only paths matching the
// agent-side allowlist may be read, and only a bounded window (default 64 KiB,
// max 1 MiB) is returned. The window can be the file head (default), a byte
// offset, or the tail (from_end / tail_lines) — the operator's "work backwards
// from the crash" path.
//
// The returned window is emitted as a searchable artifact (reachable via
// search_artifact on the hub); the inline result carries metadata only, so a
// large read never inflates the LLM context. This mirrors the journal_tail
// collector's data+artifact split.
package files

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/vasyakrg/recon/internal/agent/collect"
)

func init() { collect.Register(&fileRead{}) }

type fileRead struct{}

type FileResult struct {
	Path      string `json:"path"`
	SizeB     int64  `json:"size_bytes"`
	Offset    int64  `json:"offset,omitempty"` // start byte of the returned window
	FromEnd   bool   `json:"from_end,omitempty"`
	Bytes     int    `json:"bytes_returned"`
	LineCount int    `json:"line_count"` // newlines within the returned window
	// SHA256 is the digest of the WHOLE file, set only for a head read (the
	// only mode that scans the full file). Windowed reads (offset/from_end/
	// tail_lines) hold a non-prefix slice, so a true full-file digest would
	// need a second full pass — they expose the window digest instead.
	SHA256    string `json:"sha256,omitempty"`
	WindowSHA string `json:"sha256_of_returned_bytes,omitempty"`
	Binary    bool   `json:"binary,omitempty"`
	Artifact  string `json:"artifact"` // on-disk-safe artifact name; grep it via search_artifact
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
		Version:     "1.2.0",
		Category:    "files",
		Description: "Read a bounded window of a file from a small, hard-coded allowlist (/etc, /proc, /sys, /var/log, /run) with a denylist for secrets. The window is the head by default, or the tail (from_end / tail_lines) or a byte offset — use the tail to work backwards from an incident. Returns metadata inline and the window as a searchable artifact (grep it with search_artifact).",
		Reads:       []string{"allowlisted file paths only"},
		ParamsSchema: []collect.ParamSpec{
			{Name: "path", Type: "string", Required: true, Description: "absolute path within the allowlist"},
			{Name: "max_bytes", Type: "int", Default: "65536", Description: "max bytes to return (cap 1048576)"},
			{Name: "offset", Type: "int", Default: "0", Description: "start reading at this byte offset (mutually exclusive with from_end/tail_lines)"},
			{Name: "from_end", Type: "bool", Default: "false", Description: "read the LAST max_bytes of the file instead of the head — work backwards from the end"},
			{Name: "tail_lines", Type: "int", Default: "0", Description: "return only the last N lines of the tail window (implies from_end)"},
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
	if err := guardPath(clean); err != nil {
		return collect.Result{}, err
	}

	maxBytes := 65536
	if s := p["max_bytes"]; s != "" {
		if n, err := strconv.Atoi(s); err == nil && n > 0 && n <= 1024*1024 {
			maxBytes = n
		}
	}
	offset := int64(0)
	if s := p["offset"]; s != "" {
		if n, err := strconv.ParseInt(s, 10, 64); err == nil && n > 0 {
			offset = n
		}
	}
	fromEnd := p["from_end"] == "true" || p["from_end"] == "1"
	tailLines := 0
	if s := p["tail_lines"]; s != "" {
		if n, err := strconv.Atoi(s); err == nil && n > 0 {
			tailLines = n
			if tailLines > 100000 {
				tailLines = 100000
			}
		}
	}
	readFromEnd := fromEnd || tailLines > 0
	if offset > 0 && readFromEnd {
		return collect.Result{}, fmt.Errorf("offset cannot be combined with from_end/tail_lines")
	}
	windowed := offset > 0 || readFromEnd

	f, err := os.Open(clean)
	if err != nil {
		return collect.Result{}, fmt.Errorf("open: %w", err)
	}
	defer func() { _ = f.Close() }()

	st, err := f.Stat()
	if err != nil {
		return collect.Result{}, fmt.Errorf("stat: %w", err)
	}
	size := st.Size() // snapshot once — a growing log must not desync offset/len

	var (
		window    []byte
		startOff  int64
		fullSHA   string
		windowSHA string
		readErr   error
		readBuf   = make([]byte, maxBytes)
	)

	switch {
	case !windowed:
		// Head read: a single forward pass also yields the true full-file
		// sha256 (hash the window, then stream the remainder through it).
		hasher := sha256.New()
		var n int
		n, readErr = io.ReadFull(f, readBuf)
		if readErr != nil && readErr != io.ErrUnexpectedEOF && readErr != io.EOF {
			return collect.Result{}, fmt.Errorf("read: %w", readErr)
		}
		hasher.Write(readBuf[:n])
		if n == maxBytes {
			if _, err := io.Copy(hasher, f); err != nil {
				return collect.Result{}, fmt.Errorf("read tail for hash: %w", err)
			}
		}
		window = append([]byte(nil), readBuf[:n]...)
		fullSHA = hex.EncodeToString(hasher.Sum(nil))
	default:
		if readFromEnd {
			startOff = size - int64(maxBytes)
			if startOff < 0 {
				startOff = 0
			}
		} else {
			startOff = offset
		}
		if _, err := f.Seek(startOff, io.SeekStart); err != nil {
			return collect.Result{}, fmt.Errorf("seek: %w", err)
		}
		var n int
		n, readErr = io.ReadFull(f, readBuf)
		if readErr != nil && readErr != io.ErrUnexpectedEOF && readErr != io.EOF {
			return collect.Result{}, fmt.Errorf("read: %w", readErr)
		}
		window = append([]byte(nil), readBuf[:n]...)
	}

	// tail_lines: trim to the last N whole lines. Drop a partial leading line
	// when the window did not start at a line boundary (i.e. mid-file), so the
	// model never sees a truncated first line as if it were complete.
	if tailLines > 0 {
		if startOff > 0 {
			if i := bytes.IndexByte(window, '\n'); i >= 0 {
				window = window[i+1:]
			}
		}
		window = lastNLines(window, tailLines)
	}

	isBinary := bytes.IndexByte(window, 0) >= 0
	if windowed {
		sum := sha256.Sum256(window)
		windowSHA = hex.EncodeToString(sum[:])
	}
	artName := artifactName(clean)
	mime := "text/plain; charset=utf-8"
	if isBinary {
		mime = "application/octet-stream"
	}

	res := collect.Result{
		Data: FileResult{
			Path:      clean,
			SizeB:     size,
			Offset:    startOff,
			FromEnd:   readFromEnd,
			Bytes:     len(window),
			LineCount: bytes.Count(window, []byte{'\n'}),
			SHA256:    fullSHA,
			WindowSHA: windowSHA,
			Binary:    isBinary,
			Artifact:  artName,
		},
		Artifacts: []collect.Artifact{{Name: artName, Mime: mime, Body: window}},
	}
	if isBinary {
		res.Hints = append(res.Hints, collect.Hint{
			Severity: "info", Code: "file_read.binary",
			Message: "file appears binary (NUL bytes present); search_artifact will not return meaningful line matches",
		})
	}
	slog.Debug("collector file_read",
		"path", clean, "max_bytes", maxBytes, "offset", startOff, "from_end", readFromEnd,
		"tail_lines", tailLines, "bytes_returned", len(window), "size_bytes", size,
		"artifact", artName, "binary", isBinary)
	return res, nil
}

// lastNLines returns the last n lines of b, preserving a trailing newline.
func lastNLines(b []byte, n int) []byte {
	if n <= 0 || len(b) == 0 {
		return b
	}
	trailingNL := b[len(b)-1] == '\n'
	body := b
	if trailingNL {
		body = b[:len(b)-1]
	}
	lines := bytes.Split(body, []byte{'\n'})
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	out := bytes.Join(lines, []byte{'\n'})
	if trailingNL {
		out = append(out, '\n')
	}
	return out
}

// guardPath enforces the allowlist/denylist and refuses symlinks. Exported
// within the package so the in-process log_search collector (Task 5) reuses the
// exact same security-critical check rather than duplicating it.
//
// (H1) An attacker with write access inside any allowlist directory could
// symlink to /etc/shadow and bypass the denylist (which is purely lexical on
// the input path). Lstat the leaf AND verify EvalSymlinks didn't change it.
func guardPath(clean string) error {
	if !pathAllowed(clean) {
		return fmt.Errorf("path %q not in allowlist", clean)
	}
	li, err := os.Lstat(clean)
	if err != nil {
		return fmt.Errorf("lstat: %w", err)
	}
	if li.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("path %q is a symlink — refusing to follow", clean)
	}
	resolved, err := filepath.EvalSymlinks(clean)
	if err != nil {
		return fmt.Errorf("eval symlinks: %w", err)
	}
	if resolved != clean {
		return fmt.Errorf("path %q resolves to %q via symlinks — refusing", clean, resolved)
	}
	return nil
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

// artifactName builds a hub-on-disk-safe artifact name from a path. It applies
// the same character class the hub runner uses when it sanitizes artifact
// names on write, so the emitted name equals the name search_artifact must be
// called with (no 404 from a name the hub silently rewrote).
func artifactName(path string) string {
	return "file_read_" + sanitizeArtifact(filepath.Base(path)) + ".txt"
}

func sanitizeArtifact(s string) string {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9',
			c == '.', c == '-', c == '_':
			out = append(out, c)
		default:
			out = append(out, '_')
		}
	}
	if len(out) == 0 {
		return "file"
	}
	return string(out)
}
