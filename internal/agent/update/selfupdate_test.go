package update

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestReleaseAPIURL(t *testing.T) {
	cases := []struct {
		in   string
		want string
		ok   bool
	}{
		{"https://github.com/vasyakrg/reconops", "https://api.github.com/repos/vasyakrg/reconops/releases/latest", true},
		{"https://github.com/vasyakrg/reconops/", "https://api.github.com/repos/vasyakrg/reconops/releases/latest", true},
		{"https://hub.example.com:8443", "https://hub.example.com:8443/releases/latest", true},
		{"https://hub.example.com:8443/", "https://hub.example.com:8443/releases/latest", true},
		{"http://10.0.0.5:8080", "http://10.0.0.5:8080/releases/latest", true},
		{"ftp://nope", "", false},
		{"", "", false},
	}
	for _, c := range cases {
		got, ok := releaseAPIURL(c.in)
		if ok != c.ok || got != c.want {
			t.Fatalf("releaseAPIURL(%q)=(%q,%v) want (%q,%v)", c.in, got, ok, c.want, c.ok)
		}
	}
}

func TestNew_HubMode(t *testing.T) {
	u := New(Options{RepoURL: "https://hub.local:8443", BinaryPath: "/usr/local/bin/recon-agent"}, testLogger())
	if u == nil {
		t.Fatal("New returned nil for a hub base URL")
	}
	if u.apiURL != "https://hub.local:8443/releases/latest" {
		t.Fatalf("apiURL = %q", u.apiURL)
	}
}

// fakeTarball builds a gzip tarball with the layout the extractor expects for
// the current arch: recon-agent-linux-<arch>/bin/recon-agent.
func fakeTarball(t *testing.T, arch, payload string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	name := fmt.Sprintf("recon-agent-linux-%s/bin/recon-agent", arch)
	if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(payload))}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write([]byte(payload)); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// fakeHub serves the GitHub-API-shaped releases/latest JSON plus the tarball
// and checksums, exactly like the self-hosting hub (SH3) — so we exercise the
// agent's hub-mode fetch + SHA256-verify + extract path end to end.
func fakeHub(t *testing.T, tag string, tarball []byte) *httptest.Server {
	t.Helper()
	arch := runtime.GOARCH
	tarName := fmt.Sprintf("recon-agent-linux-%s.tar.gz", arch)
	sum := sha256.Sum256(tarball)
	checksums := fmt.Sprintf("%s  %s\n", hex.EncodeToString(sum[:]), tarName)

	mux := http.NewServeMux()
	var base string
	mux.HandleFunc("/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"tag_name":   tag,
			"draft":      false,
			"prerelease": false,
			"assets": []map[string]string{
				{"name": tarName, "browser_download_url": base + "/releases/latest/download/" + tarName},
				{"name": "checksums.txt", "browser_download_url": base + "/releases/latest/download/checksums.txt"},
			},
		})
	})
	mux.HandleFunc("/releases/latest/download/"+tarName, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(tarball)
	})
	mux.HandleFunc("/releases/latest/download/checksums.txt", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, checksums)
	})
	srv := httptest.NewServer(mux)
	base = srv.URL
	t.Cleanup(srv.Close)
	return srv
}

func TestHubMode_FetchVerifyExtract(t *testing.T) {
	arch := runtime.GOARCH
	tarball := fakeTarball(t, arch, "NEW-AGENT-BINARY")
	srv := fakeHub(t, "v9.9.9", tarball)

	binDir := t.TempDir()
	u := New(Options{
		RepoURL:    srv.URL, // hub base → hub mode
		BinaryPath: filepath.Join(binDir, "recon-agent"),
	}, testLogger())
	if u == nil {
		t.Fatal("New returned nil")
	}
	ctx := context.Background()

	rel, err := u.fetchLatest(ctx)
	if err != nil {
		t.Fatalf("fetchLatest: %v", err)
	}
	if rel.TagName != "v9.9.9" {
		t.Fatalf("tag = %q", rel.TagName)
	}
	tarName := fmt.Sprintf("recon-agent-linux-%s.tar.gz", arch)
	tarURL := assetURL(rel.Assets, tarName)
	sumURL := assetURL(rel.Assets, "checksums.txt")
	if tarURL == "" || sumURL == "" {
		t.Fatalf("missing asset urls: tar=%q sum=%q", tarURL, sumURL)
	}

	wantHash, err := u.fetchSHA256(ctx, sumURL, tarName)
	if err != nil {
		t.Fatalf("fetchSHA256: %v", err)
	}
	tmpBin, err := u.downloadAndExtract(ctx, tarURL, wantHash, arch)
	if err != nil {
		t.Fatalf("downloadAndExtract: %v", err)
	}
	defer func() { _ = os.Remove(tmpBin) }()
	got, err := os.ReadFile(tmpBin)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "NEW-AGENT-BINARY" {
		t.Fatalf("extracted binary = %q", string(got))
	}

	// swap is the last step before exit — verify it atomically replaces the path.
	if err := u.swap(tmpBin); err != nil {
		t.Fatalf("swap: %v", err)
	}
	final, err := os.ReadFile(u.opts.BinaryPath)
	if err != nil {
		t.Fatalf("read swapped binary: %v", err)
	}
	if string(final) != "NEW-AGENT-BINARY" {
		t.Fatalf("swapped binary = %q", string(final))
	}
}

func TestHubMode_SHA256MismatchRejected(t *testing.T) {
	arch := runtime.GOARCH
	tarball := fakeTarball(t, arch, "GOOD")
	srv := fakeHub(t, "v9.9.9", tarball)
	binDir := t.TempDir()
	u := New(Options{RepoURL: srv.URL, BinaryPath: filepath.Join(binDir, "recon-agent")}, testLogger())
	tarName := fmt.Sprintf("recon-agent-linux-%s.tar.gz", arch)
	tarURL := srv.URL + "/releases/latest/download/" + tarName
	// Deliberately wrong hash → download must refuse and not write the binary.
	if _, err := u.downloadAndExtract(context.Background(), tarURL, "deadbeef", arch); err == nil {
		t.Fatal("expected sha256 mismatch error, got nil")
	}
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
