// Package update implements the opt-in self-updater for recon-agent.
//
// Design notes:
//   - Opt-in per agent via `update.enabled: true` in /etc/recon/agent.yaml.
//     Default false keeps the read-only-by-default invariant: agent never
//     touches disk outside /var/lib/recon-agent unless the operator opts in.
//   - Polls GitHub Releases API (no hub round-trip — hub doesn't push
//     update commands; see HubMsg proto, which intentionally has no
//     UpdateAgent verb).
//   - Downloads recon-agent-linux-<arch>.tar.gz, verifies SHA256 against
//     checksums.txt published alongside the tarball, atomically replaces
//     the binary on disk, then exits — systemd's Restart=on-failure /
//     always policy brings it back up on the new version.
//   - On any failure the goroutine logs + retries on the next tick. A
//     broken update does not crash the running agent.
package update

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/vasyakrg/recon/internal/common/version"
)

type Options struct {
	RepoURL         string
	CheckInterval   time.Duration
	BinaryPath      string
	AllowPrerelease bool
}

type Updater struct {
	opts    Options
	apiURL  string
	log     *slog.Logger
	http    *http.Client
	current string
}

// New returns nil if opts are insufficient (disabled / bad repo URL) — the
// caller treats nil as "updater disabled" and skips the goroutine.
func New(opts Options, log *slog.Logger) *Updater {
	if opts.RepoURL == "" || opts.BinaryPath == "" {
		return nil
	}
	owner, repo, ok := parseRepo(opts.RepoURL)
	if !ok {
		return nil
	}
	if opts.CheckInterval <= 0 {
		opts.CheckInterval = time.Hour
	}
	return &Updater{
		opts:    opts,
		apiURL:  fmt.Sprintf("https://api.github.com/repos/%s/%s/releases/latest", owner, repo),
		log:     log,
		http:    &http.Client{Timeout: 2 * time.Minute}, // tarball download fits
		current: version.Version,
	}
}

// Run blocks until ctx is done. Fires an immediate check on startup so a
// freshly-restarted agent picks up a newer release without waiting a full
// CheckInterval, then ticks on CheckInterval thereafter. If a bad release
// slips out the operator sets `update.enabled: false` and restarts — the
// next tick will no-op.
func (u *Updater) Run(ctx context.Context) {
	if u == nil {
		return
	}
	u.log.Info("self-updater enabled", "interval", u.opts.CheckInterval, "binary", u.opts.BinaryPath, "current", u.current)
	if err := u.checkAndApply(ctx); err != nil {
		u.log.Warn("self-update check failed", "err", err)
	}
	t := time.NewTicker(u.opts.CheckInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := u.checkAndApply(ctx); err != nil {
				u.log.Warn("self-update check failed", "err", err)
			}
		}
	}
}

type releaseAsset struct {
	Name               string `json:"name"`
	BrowserDownloadURL string `json:"browser_download_url"`
}
type releaseJSON struct {
	TagName    string         `json:"tag_name"`
	Draft      bool           `json:"draft"`
	Prerelease bool           `json:"prerelease"`
	Assets     []releaseAsset `json:"assets"`
}

func (u *Updater) checkAndApply(ctx context.Context) error {
	rel, err := u.fetchLatest(ctx)
	if err != nil {
		return fmt.Errorf("fetch latest: %w", err)
	}
	if rel.Draft {
		return nil
	}
	if rel.Prerelease && !u.opts.AllowPrerelease {
		return nil
	}
	if !version.Outdated(u.current, rel.TagName) {
		return nil
	}
	u.log.Info("newer agent release found", "current", u.current, "latest", rel.TagName)

	arch := runtime.GOARCH
	if arch != "amd64" && arch != "arm64" {
		return fmt.Errorf("unsupported arch %q — only amd64/arm64 have published tarballs", arch)
	}
	tarName := fmt.Sprintf("recon-agent-linux-%s.tar.gz", arch)
	tarURL := assetURL(rel.Assets, tarName)
	if tarURL == "" {
		return fmt.Errorf("release %s has no asset %s", rel.TagName, tarName)
	}
	sumURL := assetURL(rel.Assets, "checksums.txt")
	if sumURL == "" {
		return fmt.Errorf("release %s has no checksums.txt — refusing to update without integrity check", rel.TagName)
	}

	wantHash, err := u.fetchSHA256(ctx, sumURL, tarName)
	if err != nil {
		return fmt.Errorf("checksum: %w", err)
	}
	tmpBin, err := u.downloadAndExtract(ctx, tarURL, wantHash, arch)
	if err != nil {
		return fmt.Errorf("download: %w", err)
	}
	defer func() { _ = os.Remove(tmpBin) }()

	if err := u.swap(tmpBin); err != nil {
		return fmt.Errorf("swap: %w", err)
	}

	u.log.Info("agent binary replaced — exiting so systemd restarts on new version", "path", u.opts.BinaryPath, "new", rel.TagName)
	// Exit the process; systemd Restart=always brings it back on the new
	// binary. We exit(0) rather than os.Exit to let defers run on the
	// caller side, but there's no clean way to cross goroutines here —
	// os.Exit is the only portable signal. Operators running without
	// systemd must restart manually; document this in agent.yaml.
	os.Exit(0)
	return nil
}

func (u *Updater) fetchLatest(ctx context.Context) (*releaseJSON, error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, u.apiURL, nil)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "recon-agent-self-updater")
	resp, err := u.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("github api %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var out releaseJSON
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out); err != nil {
		return nil, err
	}
	if out.TagName == "" {
		return nil, errors.New("empty tag_name in release json")
	}
	return &out, nil
}

// fetchSHA256 pulls checksums.txt and returns the hex digest for `name`.
// Expected format (goreleaser / shasum): "<hex>  <filename>\n".
func (u *Updater) fetchSHA256(ctx context.Context, url, name string) (string, error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	req.Header.Set("User-Agent", "recon-agent-self-updater")
	resp, err := u.http.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("checksums http %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if err != nil {
		return "", err
	}
	for _, ln := range strings.Split(string(body), "\n") {
		ln = strings.TrimSpace(ln)
		if ln == "" || strings.HasPrefix(ln, "#") {
			continue
		}
		// Two common formats: "<hex>  <name>" and "<hex> *<name>".
		fields := strings.Fields(ln)
		if len(fields) != 2 {
			continue
		}
		got := strings.TrimPrefix(fields[1], "*")
		got = filepath.Base(got)
		if got == name {
			return strings.ToLower(fields[0]), nil
		}
	}
	return "", fmt.Errorf("%s not found in checksums.txt", name)
}

func (u *Updater) downloadAndExtract(ctx context.Context, url, wantHash, arch string) (string, error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	req.Header.Set("User-Agent", "recon-agent-self-updater")
	resp, err := u.http.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("tarball http %d", resp.StatusCode)
	}

	dir := filepath.Dir(u.opts.BinaryPath)
	tmpTar, err := os.CreateTemp(dir, ".recon-agent-update-*.tgz")
	if err != nil {
		return "", err
	}
	defer func() {
		_ = tmpTar.Close()
		_ = os.Remove(tmpTar.Name())
	}()
	h := sha256.New()
	// 64MB hard cap — agent tarball is well under that; this defuses an
	// OOM attack if the URL is hijacked to serve an infinite stream.
	if _, err := io.Copy(io.MultiWriter(tmpTar, h), io.LimitReader(resp.Body, 64<<20)); err != nil {
		return "", err
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != wantHash {
		return "", fmt.Errorf("sha256 mismatch: want %s got %s", wantHash, got)
	}
	if _, err := tmpTar.Seek(0, io.SeekStart); err != nil {
		return "", err
	}
	gz, err := gzip.NewReader(tmpTar)
	if err != nil {
		return "", err
	}
	defer func() { _ = gz.Close() }()
	tr := tar.NewReader(gz)
	wantPath := fmt.Sprintf("recon-agent-linux-%s/bin/recon-agent", arch)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", err
		}
		if hdr.Name != wantPath {
			continue
		}
		tmpBin, err := os.CreateTemp(dir, ".recon-agent-new-*")
		if err != nil {
			return "", err
		}
		if _, err := io.Copy(tmpBin, io.LimitReader(tr, 128<<20)); err != nil {
			_ = tmpBin.Close()
			_ = os.Remove(tmpBin.Name())
			return "", err
		}
		if err := tmpBin.Chmod(0o755); err != nil {
			_ = tmpBin.Close()
			_ = os.Remove(tmpBin.Name())
			return "", err
		}
		if err := tmpBin.Close(); err != nil {
			return "", err
		}
		return tmpBin.Name(), nil
	}
	return "", fmt.Errorf("%s not found inside tarball", wantPath)
}

// swap replaces u.opts.BinaryPath atomically via rename(2). Requires that
// tmpBin lives on the same filesystem, which CreateTemp in filepath.Dir()
// of the target guarantees.
func (u *Updater) swap(tmpBin string) error {
	return os.Rename(tmpBin, u.opts.BinaryPath)
}

func assetURL(assets []releaseAsset, name string) string {
	for _, a := range assets {
		if a.Name == name {
			return a.BrowserDownloadURL
		}
	}
	return ""
}

func parseRepo(u string) (string, string, bool) {
	u = strings.TrimSpace(u)
	u = strings.TrimSuffix(u, "/")
	const prefix = "https://github.com/"
	if !strings.HasPrefix(u, prefix) {
		return "", "", false
	}
	rest := strings.TrimPrefix(u, prefix)
	parts := strings.SplitN(rest, "/", 3)
	if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
		return "", "", false
	}
	return parts[0], strings.TrimSuffix(parts[1], ".git"), true
}
