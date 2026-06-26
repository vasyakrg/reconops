package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestInstallConfig_Enabled(t *testing.T) {
	cases := []struct {
		name string
		cfg  InstallConfig
		want bool
	}{
		{"empty", InstallConfig{}, false},
		{"github", InstallConfig{ReleaseRepoURL: "https://github.com/x/y"}, true},
		{"self-hosted-no-repo", InstallConfig{SelfHosted: true}, true},
	}
	for _, c := range cases {
		if got := c.cfg.Enabled(); got != c.want {
			t.Fatalf("%s: Enabled()=%v want %v", c.name, got, c.want)
		}
	}
}

func TestDownloadBaseURL(t *testing.T) {
	cases := []struct {
		root, version, want string
	}{
		{"https://github.com/x/y", "latest", "https://github.com/x/y/releases/latest/download"},
		{"https://github.com/x/y", "", "https://github.com/x/y/releases/latest/download"},
		{"https://github.com/x/y/", "0.1.0", "https://github.com/x/y/releases/download/v0.1.0"},
		{"https://hub.local:8443", "v0.2.0", "https://hub.local:8443/releases/download/v0.2.0"},
		{"https://hub.local:8443", "latest", "https://hub.local:8443/releases/latest/download"},
	}
	for _, c := range cases {
		if got := downloadBaseURL(c.root, c.version); got != c.want {
			t.Fatalf("downloadBaseURL(%q,%q)=%q want %q", c.root, c.version, got, c.want)
		}
	}
}

// TestInstallScript_SelfHosted asserts the generated one-liner sources both the
// tarball (DOWNLOAD_BASE) and the agent self-updater origin (RELEASE_REPO) from
// the hub's own public base, derived from the request when external_url is empty.
func TestInstallScript_SelfHosted(t *testing.T) {
	srv, _ := newTestServer(t)
	srv.install = InstallConfig{
		SelfHosted:        true,
		ReleasesDir:       t.TempDir(),
		AgentGRPCEndpoint: "auto",
		GRPCPort:          9443,
		Version:           "latest",
	}
	tok, err := investigatorTokenFor(context.Background(), srv, "host-1", time.Hour, "test")
	if err != nil {
		t.Fatalf("issue token: %v", err)
	}
	req := httptest.NewRequest(http.MethodGet, "/install/agent.sh?token="+tok+"&id=host-1", nil)
	req.Host = "hub.local:8443"
	req.Header.Set("X-Forwarded-Proto", "https")
	rw := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rw, req)
	if rw.Code != http.StatusOK {
		t.Fatalf("want 200, got %d body=%s", rw.Code, rw.Body.String())
	}
	body := rw.Body.String()
	for _, want := range []string{
		`DOWNLOAD_BASE="https://hub.local:8443/releases/latest/download"`,
		`RELEASE_REPO="https://hub.local:8443"`,
		`HUB_ENDPOINT="hub.local:9443"`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("install script missing %q\n---\n%s", want, body)
		}
	}
}

// TestInstallScript_GitHub keeps the GitHub path working: DOWNLOAD_BASE and
// RELEASE_REPO point at the configured repo, not the hub.
func TestInstallScript_GitHub(t *testing.T) {
	srv, _ := newTestServer(t)
	srv.install = InstallConfig{
		ReleaseRepoURL:    "https://github.com/vasyakrg/reconops",
		AgentGRPCEndpoint: "hub.example.com:9443",
		Version:           "latest",
	}
	tok, err := investigatorTokenFor(context.Background(), srv, "host-2", time.Hour, "test")
	if err != nil {
		t.Fatalf("issue token: %v", err)
	}
	req := httptest.NewRequest(http.MethodGet, "/install/agent.sh?token="+tok+"&id=host-2", nil)
	req.Host = "hub.local:8443"
	rw := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rw, req)
	if rw.Code != http.StatusOK {
		t.Fatalf("want 200, got %d body=%s", rw.Code, rw.Body.String())
	}
	body := rw.Body.String()
	if !strings.Contains(body, `DOWNLOAD_BASE="https://github.com/vasyakrg/reconops/releases/latest/download"`) {
		t.Fatalf("github DOWNLOAD_BASE missing\n%s", body)
	}
	if !strings.Contains(body, `RELEASE_REPO="https://github.com/vasyakrg/reconops"`) {
		t.Fatalf("github RELEASE_REPO missing\n%s", body)
	}
}
