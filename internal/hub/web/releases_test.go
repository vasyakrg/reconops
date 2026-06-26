package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vasyakrg/recon/internal/common/version"
)

// newReleasesServer boots a Server whose only configured surface is the
// self-hosted releases dir, seeded with fake artifacts. No store/runner/LLM.
func newReleasesServer(t *testing.T, dir string) *Server {
	t.Helper()
	srv, _ := newTestServer(t)
	srv.install = InstallConfig{ReleasesDir: dir, SelfHosted: true}
	return srv
}

func seedReleases(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, f := range []struct{ name, body string }{
		{"recon-agent-linux-amd64.tar.gz", "FAKE-AMD64-TARBALL"},
		{"recon-agent-linux-arm64.tar.gz", "FAKE-ARM64-TARBALL"},
		{"checksums.txt", "deadbeef  recon-agent-linux-amd64.tar.gz\n"},
	} {
		if err := os.WriteFile(filepath.Join(dir, f.name), []byte(f.body), 0o644); err != nil {
			t.Fatalf("seed %s: %v", f.name, err)
		}
	}
	return dir
}

func TestReleases_ServeTarball(t *testing.T) {
	srv := newReleasesServer(t, seedReleases(t))
	req := httptest.NewRequest(http.MethodGet, "/releases/latest/download/recon-agent-linux-amd64.tar.gz", nil)
	rw := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rw, req)
	if rw.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rw.Code)
	}
	if ct := rw.Header().Get("Content-Type"); ct != "application/gzip" {
		t.Fatalf("content-type = %q, want application/gzip", ct)
	}
	if rw.Body.String() != "FAKE-AMD64-TARBALL" {
		t.Fatalf("body = %q", rw.Body.String())
	}
}

func TestReleases_ServeChecksums(t *testing.T) {
	srv := newReleasesServer(t, seedReleases(t))
	req := httptest.NewRequest(http.MethodGet, "/releases/latest/download/checksums.txt", nil)
	rw := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rw, req)
	if rw.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rw.Code)
	}
	if ct := rw.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Fatalf("content-type = %q, want text/plain", ct)
	}
}

func TestReleases_DownloadByTag(t *testing.T) {
	srv := newReleasesServer(t, seedReleases(t))
	// Matching tag (with a leading v) serves the artifact.
	ok := "/releases/download/v" + strings.TrimPrefix(version.Version, "v") + "/recon-agent-linux-arm64.tar.gz"
	rw := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rw, httptest.NewRequest(http.MethodGet, ok, nil))
	if rw.Code != http.StatusOK {
		t.Fatalf("matching tag: want 200, got %d", rw.Code)
	}
	// A tag that doesn't match the single bundled version → 404.
	rw = httptest.NewRecorder()
	srv.Routes().ServeHTTP(rw, httptest.NewRequest(http.MethodGet, "/releases/download/v9.9.9/recon-agent-linux-arm64.tar.gz", nil))
	if rw.Code != http.StatusNotFound {
		t.Fatalf("mismatched tag: want 404, got %d", rw.Code)
	}
}

func TestReleases_UnknownAsset404(t *testing.T) {
	srv := newReleasesServer(t, seedReleases(t))
	for _, p := range []string{
		"/releases/latest/download/evil.txt",
		"/releases/latest/download/",
		"/releases/download/onlytag",
		"/releases/bogus",
	} {
		rw := httptest.NewRecorder()
		srv.Routes().ServeHTTP(rw, httptest.NewRequest(http.MethodGet, p, nil))
		if rw.Code != http.StatusNotFound {
			t.Fatalf("%s: want 404, got %d", p, rw.Code)
		}
	}
}

func TestReleases_AssetMissingDir404(t *testing.T) {
	srv := newReleasesServer(t, t.TempDir()) // dir exists but has no artifacts
	rw := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rw, httptest.NewRequest(http.MethodGet, "/releases/latest/download/recon-agent-linux-amd64.tar.gz", nil))
	if rw.Code != http.StatusNotFound {
		t.Fatalf("want 404, got %d", rw.Code)
	}
}

func TestReleases_LatestJSON(t *testing.T) {
	srv := newReleasesServer(t, seedReleases(t))
	req := httptest.NewRequest(http.MethodGet, "/releases/latest", nil)
	req.Host = "hub.example.com:8443"
	req.Header.Set("X-Forwarded-Proto", "https")
	rw := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rw, req)
	if rw.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rw.Code)
	}
	if ct := rw.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("content-type = %q", ct)
	}
	var got struct {
		TagName    string `json:"tag_name"`
		Draft      bool   `json:"draft"`
		Prerelease bool   `json:"prerelease"`
		Assets     []struct {
			Name               string `json:"name"`
			BrowserDownloadURL string `json:"browser_download_url"`
		} `json:"assets"`
	}
	if err := json.Unmarshal(rw.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v body=%s", err, rw.Body.String())
	}
	if got.TagName != version.Version {
		t.Fatalf("tag_name = %q, want %q", got.TagName, version.Version)
	}
	if got.Draft || got.Prerelease {
		t.Fatalf("draft/prerelease must be false: %+v", got)
	}
	if len(got.Assets) != 3 {
		t.Fatalf("want 3 assets, got %d", len(got.Assets))
	}
	want := "https://hub.example.com:8443/releases/latest/download/recon-agent-linux-amd64.tar.gz"
	if got.Assets[0].BrowserDownloadURL != want {
		t.Fatalf("asset url = %q, want %q", got.Assets[0].BrowserDownloadURL, want)
	}
}

// TestReleases_PublicBaseExternalURL pins that an explicit external_url wins
// over the request host when composing asset URLs.
func TestReleases_PublicBaseExternalURL(t *testing.T) {
	srv := newReleasesServer(t, seedReleases(t))
	srv.install.ExternalURL = "https://pinned.example:9000"
	req := httptest.NewRequest(http.MethodGet, "/releases/latest", nil)
	req.Host = "ignored.example:1"
	rw := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rw, req)
	if !strings.Contains(rw.Body.String(), "https://pinned.example:9000/releases/latest/download/") {
		t.Fatalf("expected pinned external_url in asset urls, got %s", rw.Body.String())
	}
}
