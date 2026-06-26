package main

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

const (
	envHubIPAddrs           = "RECON_HUB_IP_ADDRS"
	envLLMBaseURL           = "RECON_LLM_BASE_URL"
	envLLMModel             = "RECON_LLM_MODEL"
	envLLMAllowInsecureHTTP = "RECON_LLM_ALLOW_INSECURE_HTTP"
	envLLMMaxResultTokens   = "RECON_LLM_MAX_RESULT_TOKENS"
	defaultLLMURL           = "https://openrouter.ai/api/v1"
	defaultLLMName          = "anthropic/claude-sonnet-4.5"
)

type Config struct {
	Server  ServerConfig  `yaml:"server"`
	Storage StorageConfig `yaml:"storage"`
	Auth    AuthConfig    `yaml:"auth"`
	LLM     LLMConfig     `yaml:"llm"`
	Runner  RunnerConfig  `yaml:"runner"`
	Install InstallConfig `yaml:"install"`

	Investigator InvestigatorConfig `yaml:"investigator"`

	serverIPSource string
}

// InvestigatorConfig holds investigator-loop behavior toggles that are not LLM
// transport settings.
type InvestigatorConfig struct {
	Priors PriorsConfig `yaml:"priors"`
	// RerankIntervalSteps tunes the differential re-rank checkpoint cadence (in
	// probing tool calls since the last checkpoint). Pointer so an unset key
	// keeps the compiled-in default (on); an explicit 0 disables it.
	RerankIntervalSteps *int `yaml:"rerank_interval_steps"`
}

// PriorsConfig (hub.yaml investigator.priors.*) controls the cross-investigation
// priors digest injected into a new investigation. Enabled is a pointer so an
// unset key keeps the compiled-in default (on) while an explicit `false`
// disables it; zero numeric values mean "use the compiled-in default".
type PriorsConfig struct {
	Enabled                     *bool  `yaml:"enabled"`
	MaxInvestigations           int    `yaml:"max_investigations"`
	MaxFindingsPerInvestigation int    `yaml:"max_findings_per_investigation"`
	Scope                       string `yaml:"scope"`
	MaxAgeDays                  int    `yaml:"max_age_days"`
}

// InstallConfig populates the "Quick install" one-liner shown in the hub UI.
// Both fields are deployment-specific and have no safe defaults — the operator
// must supply them before issuing install URLs.
type InstallConfig struct {
	// ReleaseRepoURL is the GitHub repo whose releases ship the agent
	// tarball, e.g. "https://github.com/vasyakrg/reconops". The hub
	// composes the actual download URL based on Version:
	//   latest   → <repo>/releases/latest/download/recon-agent-linux-<arch>.tar.gz
	//   v0.1.0   → <repo>/releases/download/v0.1.0/recon-agent-linux-<arch>.tar.gz
	ReleaseRepoURL string `yaml:"release_repo_url"`
	// AgentGRPCEndpoint is host:port the agent should configure as its
	// hub.endpoint. Set to "auto" (or leave empty) to derive from the
	// install URL's request hostname plus GRPCPort — works on the common
	// compose / single-VM case where the UI and the gRPC port live on the
	// same host. Otherwise pin to a hostname agents can resolve, e.g.
	// "hub.example.com:9443".
	AgentGRPCEndpoint string `yaml:"agent_grpc_endpoint"`
	// GRPCPort is the port number agents dial when AgentGRPCEndpoint is
	// "auto"-derived. Defaults to 9443.
	GRPCPort int `yaml:"grpc_port"`
	// ExternalURL is the public hub URL the install one-liner uses
	// (scheme + host + optional port). Setting this explicitly is the
	// reliable way to make installs work when the operator browser path
	// (e.g. orbstack auto-routing on 443) and the agent network path
	// (typically the published nginx port like 8443) differ. Empty
	// falls back to deriving from the request — which only works when
	// the same URL is reachable from both sides.
	ExternalURL string `yaml:"external_url"`
	// TrustedTLS=true drops the `-k` (insecure) flag from the install
	// one-liner. Set to true once the hub is fronted by a CA-issued
	// cert so the script fetch is verified. Default false to keep the
	// self-signed compose default working out of the box.
	TrustedTLS bool `yaml:"trusted_tls"`
	// Version selects which release the install script pulls from. Defaults
	// to "latest" so operators get the most recent published release;
	// override to a tag (e.g. "0.1.0") to pin a specific build.
	Version string `yaml:"version"`
	// ReleasesDir is the on-disk directory the hub serves agent tarballs +
	// checksums from at /releases/... (self-hosted distribution, SH1/SH2).
	// Baked into the hub image at /usr/local/share/recon/releases; override
	// only for tests / non-default layouts. Defaults to defaultReleasesDir.
	ReleasesDir string `yaml:"releases_dir"`
	// SelfHosted=true serves the agent from the hub itself (no GitHub): the
	// install one-liner and the agent self-updater are pointed at the hub's
	// own /releases route, and the outdated badge compares against the
	// bundled agent version. GitHub distribution stays selectable when false
	// (SH5). Env: RECON_INSTALL_SELF_HOSTED.
	SelfHosted bool `yaml:"self_hosted"`
}

// defaultReleasesDir is where the Dockerfile bakes the agent tarballs +
// checksums.txt the hub self-hosts. Kept in sync with the COPY target in
// Dockerfile (hub-runtime stage).
const defaultReleasesDir = "/usr/local/share/recon/releases"

// LLMConfig drives the investigator's Claude / OpenAI-compatible client.
// Defaults target OpenRouter; any of base_url / model / api_key_env may be
// overridden in hub.yaml. The actual API key is always read from env at
// runtime so it never lands in the config file (PROJECT.md §9.5).
type LLMConfig struct {
	BaseURL                   string `yaml:"base_url"`
	Model                     string `yaml:"model"`
	APIKeyEnv                 string `yaml:"api_key_env"`
	AllowInsecureHTTP         bool   `yaml:"allow_insecure_http"`
	MaxStepsPerInvestigation  int    `yaml:"max_steps_per_investigation"`
	MaxTokensPerInvestigation int    `yaml:"max_tokens_per_investigation"`
	// MaxResultTokens caps the assembled collect / collect_batch /
	// search_artifact tool result the investigator returns to the LLM (Task 1).
	// 0 falls back to the compiled default (2000). Lower it to tighten token
	// spend on huge log surveys; raise it if you want fuller per-result detail.
	MaxResultTokens int `yaml:"max_result_tokens"`
	// HistoryKeepRecentResults / HistoryDemoteMinBytes tune aged-tool-result
	// demotion in the live LLM context (Task 3): how many of the most recent
	// tool results stay verbatim, and the smallest result body worth replacing
	// with a one-line re-read pointer. 0 falls back to the compiled defaults
	// (6 / 1024). Demotion is view-only — stored results stay full.
	HistoryKeepRecentResults int `yaml:"history_keep_recent_results"`
	HistoryDemoteMinBytes    int `yaml:"history_demote_min_bytes"`
	// AutodetectContextWindow enables a best-effort GET /models probe at startup
	// to learn the real context window for profiles where the operator did NOT
	// set context_window_tokens (OpenRouter context_length / vLLM max_model_len).
	// nil = default ON; set false to disable the extra GET (air-gapped/strict).
	// Operator-set context_window_tokens is never overridden.
	AutodetectContextWindow *bool              `yaml:"autodetect_context_window"`
	HTTPReferer             string             `yaml:"http_referer"` // OpenRouter ranking header (optional)
	XTitle                  string             `yaml:"x_title"`      // OpenRouter ranking header (optional)
	Profiles                []LLMProfileConfig `yaml:"profiles"`
}

type LLMProfileConfig struct {
	Name                string `yaml:"name"`
	Role                string `yaml:"role"`
	Model               string `yaml:"model"`
	BaseURL             string `yaml:"base_url"`
	APIKeyEnv           string `yaml:"api_key_env"`
	ContextWindowTokens int    `yaml:"context_window_tokens"`
	MaxOutputTokens     int    `yaml:"max_output_tokens"`
	CostHint            string `yaml:"cost_hint"`
	SupportsTools       bool   `yaml:"supports_tools"`
	SupportsPromptCache bool   `yaml:"supports_prompt_cache"`
	AllowInsecureHTTP   bool   `yaml:"allow_insecure_http"`
	HTTPReferer         string `yaml:"http_referer"`
	XTitle              string `yaml:"x_title"`

	ContextWindowFallback bool `yaml:"-"`
}

type ServerConfig struct {
	GRPCAddr string        `yaml:"grpc_addr"`
	HTTPAddr string        `yaml:"http_addr"`
	DNSNames []string      `yaml:"dns_names"`
	IPs      []string      `yaml:"ip_addrs"`
	HTTPTLS  HTTPTLSConfig `yaml:"http_tls"`
}

// HTTPTLSConfig enables native TLS on the web/API listener so the hub can
// be reached directly from remote operators / MCP clients without nginx in
// front. When Enabled is false (default) the listener serves plain HTTP —
// the expected topology is still "nginx terminates TLS" for most prod
// deployments, but this option exists for single-VM / remote-API setups.
type HTTPTLSConfig struct {
	Enabled  bool   `yaml:"enabled"`
	CertFile string `yaml:"cert_file"`
	KeyFile  string `yaml:"key_file"`
}

type StorageConfig struct {
	DBPath        string `yaml:"db_path"`
	ArtifactDir   string `yaml:"artifact_dir"`
	CADir         string `yaml:"ca_dir"`
	RetentionDays int    `yaml:"retention_days"`
}

type RunnerConfig struct {
	PerAgentRPM int `yaml:"per_agent_rpm"` // collects/min cap; 0 = default 30
}

type AuthConfig struct {
	AdminUsers []string `yaml:"admin_users"`
}

func LoadConfig(path string) (*Config, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	cfg := &Config{}
	if err := yaml.Unmarshal(body, cfg); err != nil {
		return nil, fmt.Errorf("parse yaml: %w", err)
	}
	if cfg.Server.GRPCAddr == "" {
		cfg.Server.GRPCAddr = ":9443"
	}
	if cfg.Server.HTTPAddr == "" {
		// (M5) Web UI has no auth in MVP. Default to loopback so a fresh
		// install does not leak host inventory. Operators must explicitly
		// override to expose it (typically behind nginx + auth — Week 5).
		cfg.Server.HTTPAddr = "127.0.0.1:8080"
	}
	if rawIPs := strings.TrimSpace(os.Getenv(envHubIPAddrs)); rawIPs != "" {
		ips, err := parseIPCSV(rawIPs)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", envHubIPAddrs, err)
		}
		cfg.Server.IPs = ips
		cfg.serverIPSource = "env:" + envHubIPAddrs
	} else if len(cfg.Server.IPs) > 0 {
		cfg.serverIPSource = "yaml:server.ip_addrs"
	} else {
		cfg.serverIPSource = "unset"
	}
	if _, err := parseIPStrings(cfg.Server.IPs); err != nil {
		return nil, fmt.Errorf("server.ip_addrs: %w", err)
	}
	if cfg.Storage.DBPath == "" {
		cfg.Storage.DBPath = "/var/lib/recon/recon.db"
	}
	if cfg.Storage.ArtifactDir == "" {
		cfg.Storage.ArtifactDir = "/var/lib/recon/artifacts"
	}
	if cfg.Storage.CADir == "" {
		cfg.Storage.CADir = filepath.Join(filepath.Dir(cfg.Storage.DBPath), "ca")
	}
	if cfg.Storage.RetentionDays == 0 {
		cfg.Storage.RetentionDays = 30
	}
	// LLM defaults — env vars always win over yaml; yaml wins over compiled
	// defaults. Final concrete values are resolved in main via env lookup.
	if v := strings.TrimSpace(os.Getenv(envLLMBaseURL)); v != "" {
		cfg.LLM.BaseURL = v
	} else if cfg.LLM.BaseURL == "" {
		cfg.LLM.BaseURL = defaultLLMURL
	}
	if v := strings.TrimSpace(os.Getenv(envLLMModel)); v != "" {
		cfg.LLM.Model = v
	} else if cfg.LLM.Model == "" {
		cfg.LLM.Model = defaultLLMName
	}
	if cfg.LLM.APIKeyEnv == "" {
		cfg.LLM.APIKeyEnv = "RECON_LLM_API_KEY"
	}
	if rawAllow := strings.TrimSpace(os.Getenv(envLLMAllowInsecureHTTP)); rawAllow != "" {
		allow, err := parseBoolEnv(envLLMAllowInsecureHTTP, rawAllow)
		if err != nil {
			return nil, err
		}
		cfg.LLM.AllowInsecureHTTP = allow
	}
	if cfg.LLM.MaxStepsPerInvestigation == 0 {
		// (accuracy) 12 is a forcing function: investigations that cannot
		// terminate within 12 tool_calls usually stopped being productive
		// a while ago. Operators can always click "extend" to buy more.
		cfg.LLM.MaxStepsPerInvestigation = 12
	}
	if cfg.LLM.MaxTokensPerInvestigation == 0 {
		cfg.LLM.MaxTokensPerInvestigation = 500_000
	}
	// Env override wins over yaml, consistent with the other RECON_LLM_* knobs.
	if raw := strings.TrimSpace(os.Getenv(envLLMMaxResultTokens)); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 0 {
			return nil, fmt.Errorf("%s must be a non-negative integer, got %q", envLLMMaxResultTokens, raw)
		}
		cfg.LLM.MaxResultTokens = n
	}
	if cfg.LLM.MaxResultTokens == 0 {
		// PROJECT.md §7.4 targets ~500–2000 tokens per tool result. 2000 keeps
		// fleet-wide log surveys bounded while still leaving room for a useful
		// per-host headline + top clusters.
		cfg.LLM.MaxResultTokens = 2000
	}
	if err := cfg.LLM.resolveProfiles(); err != nil {
		return nil, err
	}
	if cfg.Install.Version == "" {
		cfg.Install.Version = envOr("RECON_INSTALL_VERSION", "latest")
	}
	if cfg.Install.ReleaseRepoURL == "" {
		cfg.Install.ReleaseRepoURL = envOr("RECON_INSTALL_RELEASE_REPO", "")
	}
	if cfg.Install.AgentGRPCEndpoint == "" {
		cfg.Install.AgentGRPCEndpoint = envOr("RECON_INSTALL_GRPC_ENDPOINT", "auto")
	}
	if cfg.Install.GRPCPort == 0 {
		cfg.Install.GRPCPort = 9443
	}
	if cfg.Install.ExternalURL == "" {
		cfg.Install.ExternalURL = envOr("RECON_INSTALL_EXTERNAL_URL", "")
	}
	if cfg.Install.ReleasesDir == "" {
		cfg.Install.ReleasesDir = envOr("RECON_INSTALL_RELEASES_DIR", defaultReleasesDir)
	}
	if !cfg.Install.SelfHosted {
		cfg.Install.SelfHosted = envOr("RECON_INSTALL_SELF_HOSTED", "") == "true"
	}
	if !cfg.Install.TrustedTLS {
		cfg.Install.TrustedTLS = envOr("RECON_INSTALL_TRUSTED_TLS", "") == "true"
	}
	return cfg, nil
}

// AutodetectContextWindowEnabled reports whether startup should probe GET
// /models for the context window. Defaults to true when unset.
func (c LLMConfig) AutodetectContextWindowEnabled() bool {
	return c.AutodetectContextWindow == nil || *c.AutodetectContextWindow
}

func (c LLMConfig) PrimaryProfile() LLMProfileConfig {
	for _, p := range c.Profiles {
		if p.Role == "primary" {
			return p
		}
	}
	if len(c.Profiles) > 0 {
		return c.Profiles[0]
	}
	return LLMProfileConfig{}
}

func (c *LLMConfig) resolveProfiles() error {
	legacy := LLMProfileConfig{
		Name:              "primary",
		Role:              "primary",
		Model:             c.Model,
		BaseURL:           c.BaseURL,
		APIKeyEnv:         c.APIKeyEnv,
		MaxOutputTokens:   4096,
		SupportsTools:     true,
		AllowInsecureHTTP: c.AllowInsecureHTTP,
		HTTPReferer:       c.HTTPReferer,
		XTitle:            c.XTitle,
	}
	if len(c.Profiles) == 0 {
		c.Profiles = []LLMProfileConfig{legacy}
	} else {
		hasPrimary := false
		for i := range c.Profiles {
			if strings.TrimSpace(c.Profiles[i].Role) == "primary" {
				hasPrimary = true
				break
			}
		}
		if !hasPrimary {
			c.Profiles = append([]LLMProfileConfig{legacy}, c.Profiles...)
		}
	}
	seen := map[string]bool{}
	for i := range c.Profiles {
		p := &c.Profiles[i]
		p.Name = strings.TrimSpace(p.Name)
		p.Role = strings.TrimSpace(p.Role)
		if p.Role == "" {
			p.Role = "primary"
		}
		if p.Name == "" {
			p.Name = p.Role
		}
		if seen[p.Name] {
			return fmt.Errorf("llm.profiles: duplicate profile name %q", p.Name)
		}
		seen[p.Name] = true
		switch p.Role {
		case "primary", "summarizer", "triage", "verifier", "cheap":
		default:
			return fmt.Errorf("llm.profiles[%s].role: unsupported role %q", p.Name, p.Role)
		}
		if p.Model == "" {
			p.Model = c.Model
		}
		if p.BaseURL == "" {
			p.BaseURL = c.BaseURL
		}
		if p.APIKeyEnv == "" {
			p.APIKeyEnv = c.APIKeyEnv
		}
		if p.HTTPReferer == "" {
			p.HTTPReferer = c.HTTPReferer
		}
		if p.XTitle == "" {
			p.XTitle = c.XTitle
		}
		if !p.AllowInsecureHTTP {
			p.AllowInsecureHTTP = c.AllowInsecureHTTP
		}
		if p.MaxOutputTokens == 0 {
			p.MaxOutputTokens = 4096
		}
		if p.ContextWindowTokens <= 0 {
			p.ContextWindowTokens = fallbackContextWindowTokens(p.Model)
			p.ContextWindowFallback = true
		}
	}
	primary := c.PrimaryProfile()
	c.Model = primary.Model
	c.BaseURL = primary.BaseURL
	c.APIKeyEnv = primary.APIKeyEnv
	c.AllowInsecureHTTP = primary.AllowInsecureHTTP
	c.HTTPReferer = primary.HTTPReferer
	c.XTitle = primary.XTitle
	return nil
}

func fallbackContextWindowTokens(model string) int {
	switch strings.TrimSpace(model) {
	case defaultLLMName:
		return 200_000
	default:
		return 128_000
	}
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func (c *Config) ParsedIPs() ([]net.IP, error) {
	return parseIPStrings(c.Server.IPs)
}

func (c *Config) ServerIPSource() string {
	if c.serverIPSource == "" {
		return "unknown"
	}
	return c.serverIPSource
}

func parseIPCSV(raw string) ([]string, error) {
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		ip := strings.TrimSpace(part)
		if ip == "" {
			continue
		}
		out = append(out, ip)
	}
	if _, err := parseIPStrings(out); err != nil {
		return nil, err
	}
	return out, nil
}

func parseIPStrings(values []string) ([]net.IP, error) {
	out := make([]net.IP, 0, len(values))
	for _, raw := range values {
		value := strings.TrimSpace(raw)
		if value == "" {
			continue
		}
		ip := net.ParseIP(value)
		if ip == nil {
			return nil, fmt.Errorf("invalid IP address %q", raw)
		}
		out = append(out, ip)
	}
	return out, nil
}

func parseBoolEnv(key, raw string) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "1", "t", "true", "y", "yes", "on":
		return true, nil
	case "0", "f", "false", "n", "no", "off":
		return false, nil
	default:
		return false, fmt.Errorf("%s must be a boolean, got %q", key, raw)
	}
}
