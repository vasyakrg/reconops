package files

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseMounts(t *testing.T) {
	body := `/dev/sda1 / ext4 rw,relatime 0 0
proc /proc proc rw,nosuid 0 0
/dev/sda2 /home xfs rw,relatime,attr2 0 0
tmpfs /run tmpfs rw,nosuid 0 0
`
	dir := t.TempDir()
	p := filepath.Join(dir, "mounts")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := parseMounts(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 4 {
		t.Fatalf("got %d mounts", len(got))
	}
	if got[0].FSType != "ext4" || got[2].FSType != "xfs" {
		t.Fatalf("unexpected fstypes: %+v", got)
	}
}

func TestStrconvF(t *testing.T) {
	if got := strconvF(95.234, 1); got != "95.2" {
		t.Errorf("got %q", got)
	}
	if got := strconvF(0, 1); got != "0.0" {
		t.Errorf("got %q", got)
	}
	if got := strconvF(99.999, 1); got != "100.0" {
		t.Errorf("got %q", got)
	}
}
