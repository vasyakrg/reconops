package web

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/vasyakrg/recon/internal/common/version"
)

// agentReleaseAssets whitelists the basenames the hub serves from the releases
// dir, each mapped to its Content-Type. The /releases route is public +
// unauthenticated (like /install/agent.sh), so this whitelist — together with
// filepath.Clean + the dir-prefix guard in serveReleaseAsset — is what stops
// the route from leaking arbitrary files.
var agentReleaseAssets = map[string]string{
	"recon-agent-linux-amd64.tar.gz": "application/gzip",
	"recon-agent-linux-arm64.tar.gz": "application/gzip",
	"checksums.txt":                  "text/plain; charset=utf-8",
}

// releaseAssetOrder fixes a deterministic asset order for the releases/latest
// JSON (tarballs first, then checksums) so the response is stable across calls.
var releaseAssetOrder = []string{
	"recon-agent-linux-amd64.tar.gz",
	"recon-agent-linux-arm64.tar.gz",
	"checksums.txt",
}

// bundledAgentVersion is the version of the agent tarballs baked into the hub
// image. Hub and agent are built from the same commit with the same LDFLAGS
// (see Dockerfile), so the hub's own version IS the served agent's version.
func (s *Server) bundledAgentVersion() string { return version.Version }

// handleReleases serves the self-hosted agent distribution surface (SH2/SH3)
// under /releases/:
//
//	GET /releases/latest                  → GitHub-API-shaped release JSON (SH3)
//	GET /releases/latest/download/<asset> → tarball / checksums.txt          (SH2)
//	GET /releases/download/<tag>/<asset>  → tarball / checksums.txt, tag must
//	                                        match the bundled version        (SH2)
//
// All routes are public — the bundled agent is not a secret and the install /
// self-update flow needs them before any token exchange. Unknown shapes → 404.
// The endpoint lets the agent self-updater reuse its GitHub-JSON parser and
// verified download→SHA256→atomic-swap code against the hub instead of GitHub.
func (s *Server) handleReleases(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, "/releases/")
	switch {
	case rest == "latest":
		s.serveLatestReleaseJSON(w, r)
	case strings.HasPrefix(rest, "latest/download/"):
		s.serveReleaseAsset(w, r, strings.TrimPrefix(rest, "latest/download/"))
	case strings.HasPrefix(rest, "download/"):
		// download/<tag>/<asset>
		tagAndAsset := strings.TrimPrefix(rest, "download/")
		slash := strings.IndexByte(tagAndAsset, '/')
		if slash < 0 {
			http.NotFound(w, r)
			return
		}
		tag, asset := tagAndAsset[:slash], tagAndAsset[slash+1:]
		// Only one version is bundled per image; a pinned tag that doesn't
		// match it has no artifact to serve.
		if !sameVersion(tag, s.bundledAgentVersion()) {
			s.log.Debug("release tag mismatch", "want", s.bundledAgentVersion(), "got", tag)
			http.NotFound(w, r)
			return
		}
		s.serveReleaseAsset(w, r, asset)
	default:
		http.NotFound(w, r)
	}
}

// serveLatestReleaseJSON returns a GitHub-Releases-API-shaped document for the
// bundled agent. tag_name is the bundled version; asset URLs point back at the
// SH2 download route resolved from the hub's public base (install.external_url
// or the request), so they're reachable by whatever address the agent used to
// reach this endpoint.
func (s *Server) serveLatestReleaseJSON(w http.ResponseWriter, r *http.Request) {
	base := s.publicBase(r)
	type asset struct {
		Name               string `json:"name"`
		BrowserDownloadURL string `json:"browser_download_url"`
	}
	assets := make([]asset, 0, len(releaseAssetOrder))
	for _, name := range releaseAssetOrder {
		assets = append(assets, asset{
			Name:               name,
			BrowserDownloadURL: base + "/releases/latest/download/" + name,
		})
	}
	payload := struct {
		TagName    string  `json:"tag_name"`
		Draft      bool    `json:"draft"`
		Prerelease bool    `json:"prerelease"`
		Assets     []asset `json:"assets"`
	}{
		TagName:    s.bundledAgentVersion(),
		Draft:      false,
		Prerelease: false,
		Assets:     assets,
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if err := json.NewEncoder(w).Encode(payload); err != nil {
		s.log.Warn("encode releases/latest json", "err", err)
		return
	}
	s.log.Debug("served self-hosted releases/latest", "tag", payload.TagName, "base", base)
}

// serveReleaseAsset streams a whitelisted artifact from the releases dir. name
// is validated against agentReleaseAssets (no path separators) before any
// filesystem access; the Clean + prefix guard is defence-in-depth against a
// future widening of that whitelist.
func (s *Server) serveReleaseAsset(w http.ResponseWriter, r *http.Request, name string) {
	ctype, ok := agentReleaseAssets[name]
	if !ok {
		http.NotFound(w, r)
		return
	}
	dir := s.install.ReleasesDir
	if dir == "" {
		s.log.Debug("release asset request with no releases_dir configured", "name", name)
		http.NotFound(w, r)
		return
	}
	clean := filepath.Clean(filepath.Join(dir, name))
	if !strings.HasPrefix(clean, filepath.Clean(dir)+string(os.PathSeparator)) {
		http.Error(w, "path traversal", http.StatusBadRequest)
		return
	}
	f, err := os.Open(clean)
	if err != nil {
		s.log.Debug("release asset not found on disk", "name", name, "dir", dir, "err", err)
		http.NotFound(w, r)
		return
	}
	defer func() { _ = f.Close() }()
	fi, err := f.Stat()
	if err != nil || fi.IsDir() {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", ctype)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	// ServeContent handles Range, Content-Length, conditional requests, and
	// HEAD; it won't override the Content-Type we set above.
	http.ServeContent(w, r, name, fi.ModTime(), f)
	s.log.Debug("served release asset", "name", name, "bytes", fi.Size())
}

// publicBase returns the scheme://host[:port] the install one-liner and the
// self-hosted /releases JSON advertise. Priority: explicit install.external_url,
// then nginx X-Forwarded-* headers, then the bare request host. Mirrors the
// resolution handleQuickInstall uses for the script URL so the tarball, the
// release JSON, and the script all come from the same origin the operator's
// browser (or the agent) actually hit.
func (s *Server) publicBase(r *http.Request) string {
	if base := strings.TrimRight(s.install.ExternalURL, "/"); base != "" {
		return base
	}
	scheme := "http"
	if r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" {
		scheme = "https"
	}
	host := r.Header.Get("X-Forwarded-Host")
	if host == "" {
		host = r.Host
	}
	return scheme + "://" + host
}

// sameVersion compares two version strings ignoring a leading "v" on either
// side (GitHub tags carry it; the LDFLAGS-stamped version usually doesn't).
func sameVersion(a, b string) bool {
	return strings.TrimPrefix(a, "v") == strings.TrimPrefix(b, "v")
}

// agentDownloadBase is the root origin the outdated-host update hint builds the
// manual tarball URL from: <base>/releases/download/<tag>/recon-agent-linux-<arch>.tar.gz.
// Self-hosted → the hub's public base (so the update command pulls from the hub,
// not GitHub). GitHub mode → the repo root (releasesURL with its trailing
// "/releases" stripped). Empty when neither is available; the UI's JS then falls
// back to the default GitHub repo.
func (s *Server) agentDownloadBase(r *http.Request, releasesURL string) string {
	if s.install.SelfHosted {
		return s.publicBase(r)
	}
	return strings.TrimSuffix(strings.TrimRight(releasesURL, "/"), "/releases")
}
