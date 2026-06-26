package files

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"strconv"
	"testing"

	"github.com/vasyakrg/recon/internal/agent/collect"
)

// writeProbe writes content to an allowlisted path under /var/log and returns
// it, or skips when the test lacks write permission there.
func writeProbe(t *testing.T, name string, content []byte) string {
	t.Helper()
	path := "/var/log/" + name
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Skipf("cannot write probe file at %s: %v", path, err)
	}
	t.Cleanup(func() { _ = os.Remove(path) })
	return path
}

func TestPathAllowed(t *testing.T) {
	cases := []struct {
		path string
		want bool
	}{
		{"/etc/os-release", true},
		{"/proc/cpuinfo", true},
		{"/var/log/syslog", true},
		{"/run/systemd/journal", true},
		{"/etc/shadow", false},
		{"/etc/recon/agent.yaml", false},
		{"/etc/ssl/private/server.key", false},
		{"/home/user/.ssh/id_rsa", false},
		{"/", false},
	}
	for _, c := range cases {
		if got := pathAllowed(c.path); got != c.want {
			t.Errorf("pathAllowed(%q) = %v, want %v", c.path, got, c.want)
		}
	}
}

func TestFileReadAllowedPath(t *testing.T) {
	c := fileRead{}
	res, err := c.Run(context.Background(), collect.Params{"path": "/etc/hosts", "max_bytes": "256"})
	if err != nil {
		t.Skipf("skipping — /etc/hosts unreadable on this host: %v", err)
	}
	d := res.Data.(FileResult)
	if d.SHA256 == "" || d.SizeB <= 0 {
		t.Fatalf("unexpected: %+v", d)
	}
	// The body must live in a searchable artifact, not inline in Data.
	if len(res.Artifacts) != 1 {
		t.Fatalf("expected exactly one artifact, got %d", len(res.Artifacts))
	}
	art := res.Artifacts[0]
	if art.Name != d.Artifact {
		t.Fatalf("artifact name mismatch: result says %q, artifact is %q", d.Artifact, art.Name)
	}
	if len(art.Body) != d.Bytes {
		t.Fatalf("artifact body is %d bytes, FileResult.Bytes is %d", len(art.Body), d.Bytes)
	}
}

// The hub sanitize()s artifact names on write and search_artifact joins the raw
// name; an emitted name that does not survive sanitize() re-creates the
// retrieval dead-end. Assert emitted == sanitize-stable for every emitted name.
func TestFileReadArtifactNameIsWriteStable(t *testing.T) {
	cases := []string{
		"/var/log/syslog",
		"/var/log/syslog.1",
		"/etc/os-release",
		"/proc/cpuinfo",
	}
	for _, p := range cases {
		name := artifactName(p)
		if got := sanitizeArtifact(name); got != name {
			t.Errorf("artifactName(%q)=%q is not write-stable: sanitize→%q", p, name, got)
		}
	}
	if got := artifactName("/var/log/syslog"); got != "file_read_syslog.txt" {
		t.Errorf("artifactName(/var/log/syslog) = %q, want file_read_syslog.txt", got)
	}
}

func TestFileReadDenied(t *testing.T) {
	c := fileRead{}
	if _, err := c.Run(context.Background(), collect.Params{"path": "/etc/shadow"}); err == nil {
		t.Fatal("expected denylist hit")
	}
	if _, err := c.Run(context.Background(), collect.Params{"path": "/home/user/.bashrc"}); err == nil {
		t.Fatal("expected allowlist miss")
	}
	if _, err := c.Run(context.Background(), collect.Params{"path": "../etc/passwd"}); err == nil {
		t.Fatal("expected absolute-path requirement")
	}
}

func TestFileReadRefusesSymlink(t *testing.T) {
	dir := t.TempDir()
	target := dir + "/target"
	link := "/var/log/recon_test_symlink_probe"
	if err := os.WriteFile(target, []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Try to create symlink in /var/log; if no permission, skip — the
	// security property is still asserted by the lstat check unconditionally.
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("cannot create symlink at %s: %v", link, err)
	}
	t.Cleanup(func() { _ = os.Remove(link) })

	c := fileRead{}
	if _, err := c.Run(context.Background(), collect.Params{"path": link}); err == nil {
		t.Fatal("expected refusal of symlink path")
	}
}

func TestFileReadCaps(t *testing.T) {
	c := fileRead{}
	// max_bytes way too high: parameter is silently capped.
	res, err := c.Run(context.Background(), collect.Params{"path": "/etc/hosts", "max_bytes": strconv.Itoa(1 << 30)})
	if err != nil {
		t.Skipf("skipping — /etc/hosts unreadable: %v", err)
	}
	d := res.Data.(FileResult)
	if d.Bytes > 1024*1024 {
		t.Fatalf("max_bytes not capped: %d", d.Bytes)
	}
}

// A file containing NUL bytes must be flagged binary so the hub does not regex
// it as text. Best-effort: needs write access to an allowlisted dir (/var/log).
func TestFileReadBinaryDetection(t *testing.T) {
	path := "/var/log/recon_test_fileread_binary.bin"
	if err := os.WriteFile(path, []byte{'a', 0x00, 'b', 0x00, 'c'}, 0o600); err != nil {
		t.Skipf("cannot write probe file at %s: %v", path, err)
	}
	t.Cleanup(func() { _ = os.Remove(path) })

	c := fileRead{}
	res, err := c.Run(context.Background(), collect.Params{"path": path})
	if err != nil {
		t.Fatalf("read failed: %v", err)
	}
	d := res.Data.(FileResult)
	if !d.Binary {
		t.Fatalf("expected Binary=true for NUL-containing file, got %+v", d)
	}
	if res.Artifacts[0].Mime != "application/octet-stream" {
		t.Errorf("expected octet-stream mime for binary, got %q", res.Artifacts[0].Mime)
	}
	found := false
	for _, h := range res.Hints {
		if h.Code == "file_read.binary" {
			found = true
		}
	}
	if !found {
		t.Error("expected a file_read.binary hint")
	}
}

func TestFileReadHeadFullFileSHA(t *testing.T) {
	content := []byte("L0\nL1\nL2\nL3\nL4\nL5\nL6\nL7\nL8\nL9\n")
	path := writeProbe(t, "recon_test_fileread_head.log", content)
	c := fileRead{}
	// max_bytes smaller than the file: still a head read, full-file sha256.
	res, err := c.Run(context.Background(), collect.Params{"path": path, "max_bytes": "6"})
	if err != nil {
		t.Fatal(err)
	}
	d := res.Data.(FileResult)
	want := hex.EncodeToString(func() []byte { s := sha256.Sum256(content); return s[:] }())
	if d.SHA256 != want {
		t.Fatalf("head read must report full-file sha256\n got %q\nwant %q", d.SHA256, want)
	}
	if d.WindowSHA != "" {
		t.Errorf("head read must not set windowed sha: %q", d.WindowSHA)
	}
	if d.Bytes != 6 || string(res.Artifacts[0].Body) != "L0\nL1\n" {
		t.Fatalf("unexpected head window: bytes=%d body=%q", d.Bytes, res.Artifacts[0].Body)
	}
}

func TestFileReadFromEndWindowSHA(t *testing.T) {
	content := []byte("L0\nL1\nL2\nL3\nL4\nL5\nL6\nL7\nL8\nL9\n")
	path := writeProbe(t, "recon_test_fileread_tail.log", content)
	c := fileRead{}
	res, err := c.Run(context.Background(), collect.Params{"path": path, "from_end": "true", "max_bytes": "5"})
	if err != nil {
		t.Fatal(err)
	}
	d := res.Data.(FileResult)
	wantWindow := content[len(content)-5:]
	if string(res.Artifacts[0].Body) != string(wantWindow) {
		t.Fatalf("from_end window = %q, want %q", res.Artifacts[0].Body, wantWindow)
	}
	if !d.FromEnd || d.SHA256 != "" {
		t.Errorf("from_end must not report a full-file sha256: %+v", d)
	}
	wsum := sha256.Sum256(wantWindow)
	if d.WindowSHA != hex.EncodeToString(wsum[:]) {
		t.Errorf("windowed sha mismatch: %q", d.WindowSHA)
	}
}

func TestFileReadTailLines(t *testing.T) {
	content := []byte("L0\nL1\nL2\nL3\nL4\nL5\nL6\nL7\nL8\nL9\n")
	path := writeProbe(t, "recon_test_fileread_taillines.log", content)
	c := fileRead{}
	res, err := c.Run(context.Background(), collect.Params{"path": path, "tail_lines": "2", "max_bytes": "1048576"})
	if err != nil {
		t.Fatal(err)
	}
	if got := string(res.Artifacts[0].Body); got != "L8\nL9\n" {
		t.Fatalf("tail_lines=2 = %q, want %q", got, "L8\nL9\n")
	}
}

func TestFileReadOffset(t *testing.T) {
	content := []byte("L0\nL1\nL2\nL3\n")
	path := writeProbe(t, "recon_test_fileread_offset.log", content)
	c := fileRead{}
	// offset 3 = start of "L1\n"; read 3 bytes.
	res, err := c.Run(context.Background(), collect.Params{"path": path, "offset": "3", "max_bytes": "3"})
	if err != nil {
		t.Fatal(err)
	}
	d := res.Data.(FileResult)
	if d.Offset != 3 || string(res.Artifacts[0].Body) != "L1\n" {
		t.Fatalf("offset read = off %d body %q", d.Offset, res.Artifacts[0].Body)
	}
}

func TestFileReadOffsetPastEOF(t *testing.T) {
	content := []byte("short\n")
	path := writeProbe(t, "recon_test_fileread_pasteof.log", content)
	c := fileRead{}
	res, err := c.Run(context.Background(), collect.Params{"path": path, "offset": "9999"})
	if err != nil {
		t.Fatalf("offset past EOF should not error: %v", err)
	}
	if res.Data.(FileResult).Bytes != 0 {
		t.Fatalf("offset past EOF should return 0 bytes, got %d", res.Data.(FileResult).Bytes)
	}
}

func TestFileReadOffsetAndFromEndRejected(t *testing.T) {
	c := fileRead{}
	_, err := c.Run(context.Background(), collect.Params{"path": "/etc/hosts", "offset": "10", "from_end": "true"})
	if err == nil {
		t.Fatal("offset + from_end must be rejected")
	}
}
