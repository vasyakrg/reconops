package files

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/vasyakrg/recon/internal/agent/collect"
)

func TestLogSearchBasic(t *testing.T) {
	content := []byte("alpha\nbravo needle\ncharlie\ndelta needle\n")
	path := writeProbe(t, "recon_test_ls_basic.log", content)
	c := logSearch{}
	res, err := c.Run(context.Background(), collect.Params{"path": path, "pattern": "needle", "context_lines": "0"})
	if err != nil {
		t.Fatal(err)
	}
	d := res.Data.(LogSearchSummary)
	if d.Matches != 2 {
		t.Fatalf("want 2 matches, got %d", d.Matches)
	}
	body := string(res.Artifacts[0].Body)
	if !strings.Contains(body, path+":2: bravo needle") || !strings.Contains(body, path+":4: delta needle") {
		t.Fatalf("artifact missing file:line refs:\n%s", body)
	}
	if len(res.Hints) == 0 {
		t.Error("expected sample-match hints")
	}
}

func TestLogSearchContextLines(t *testing.T) {
	content := []byte("l1\nl2\nNEEDLE\nl4\nl5\n")
	path := writeProbe(t, "recon_test_ls_ctx.log", content)
	c := logSearch{}
	res, err := c.Run(context.Background(), collect.Params{"path": path, "pattern": "NEEDLE", "context_lines": "1"})
	if err != nil {
		t.Fatal(err)
	}
	body := string(res.Artifacts[0].Body)
	if !strings.Contains(body, "| l2") || !strings.Contains(body, "| l4") {
		t.Fatalf("expected context lines l2 and l4:\n%s", body)
	}
}

func TestLogSearchMaxMatches(t *testing.T) {
	var sb strings.Builder
	for i := 0; i < 20; i++ {
		sb.WriteString("hit here\n")
	}
	path := writeProbe(t, "recon_test_ls_cap.log", []byte(sb.String()))
	c := logSearch{}
	res, err := c.Run(context.Background(), collect.Params{"path": path, "pattern": "hit", "max_matches": "2"})
	if err != nil {
		t.Fatal(err)
	}
	d := res.Data.(LogSearchSummary)
	if d.Matches != 2 || !d.Truncated {
		t.Fatalf("want 2 matches + truncated, got matches=%d truncated=%v", d.Matches, d.Truncated)
	}
}

func TestLogSearchInvalidRegex(t *testing.T) {
	c := logSearch{}
	if _, err := c.Run(context.Background(), collect.Params{"path": "/etc/hosts", "pattern": "([unclosed"}); err == nil {
		t.Fatal("invalid RE2 pattern must error (not panic)")
	}
}

func TestLogSearchDeniedPath(t *testing.T) {
	c := logSearch{}
	if _, err := c.Run(context.Background(), collect.Params{"path": "/etc/shadow", "pattern": "x"}); err == nil {
		t.Fatal("denylisted path must error")
	}
}

func TestLogSearchGlobSkipsSymlink(t *testing.T) {
	real := writeProbe(t, "recon_test_ls_real.log", []byte("needle in real\n"))
	link := "/var/log/recon_test_ls_link.log"
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("cannot create symlink: %v", err)
	}
	t.Cleanup(func() { _ = os.Remove(link) })
	c := logSearch{}
	res, err := c.Run(context.Background(), collect.Params{"path_glob": "/var/log/recon_test_ls_*.log", "pattern": "needle"})
	if err != nil {
		t.Fatal(err)
	}
	d := res.Data.(LogSearchSummary)
	// Only the regular file is scanned; the symlink match is guarded out.
	if d.FilesScanned != 1 {
		t.Fatalf("symlinked glob match must be skipped: files_scanned=%d", d.FilesScanned)
	}
}

func TestLogSearchSinceFilter(t *testing.T) {
	content := []byte("2026-06-14T00:00:00Z old needle\n2026-06-20T12:00:00Z new needle\n")
	path := writeProbe(t, "recon_test_ls_time.log", content)
	c := logSearch{}
	res, err := c.Run(context.Background(), collect.Params{
		"path": path, "pattern": "needle", "since": "2026-06-19T00:00:00Z", "context_lines": "0",
	})
	if err != nil {
		t.Fatal(err)
	}
	d := res.Data.(LogSearchSummary)
	if d.Matches != 1 {
		t.Fatalf("since filter should keep only the newer line, got %d matches", d.Matches)
	}
	if !strings.Contains(string(res.Artifacts[0].Body), "new needle") {
		t.Fatalf("expected the newer line to survive the since filter:\n%s", res.Artifacts[0].Body)
	}
}
