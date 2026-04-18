package main

import (
	"fmt"
	"net"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Server  ServerConfig  `yaml:"server"`
	Storage StorageConfig `yaml:"storage"`
	Auth    AuthConfig    `yaml:"auth"`
	LLM     LLMConfig     `yaml:"llm"`
	Runner  RunnerConfig  `yaml:"runner"`
	Install InstallConfig `yaml:"install"`
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
	// Version selects which release the install script pulls from. Defaults
	// to "latest" so operators get the most recent published release;
	// override to a tag (e.g. "0.1.0") to pin a specific build.
	Version string `yaml:"version"`
}

// LLMConfig drives the investigator's Claude / OpenAI-compatible client.
// Defaults target OpenRouter; any of base_url / model / api_key_env may be
// overridden in hub.yaml. The actual API key is always read from env at
// runtime so it never lands in the config file (PROJECT.md §9.5).
type LLMConfig struct {
	BaseURL                   string `yaml:"base_url"`
	Model                     string `yaml:"model"`
	APIKeyEnv                 string `yaml:"api_key_env"`
	MaxStepsPerInvestigation  int    `yaml:"max_steps_per_investigation"`
	MaxTokensPerInvestigation int    `yaml:"max_tokens_per_investigation"`
	HTTPReferer               string `yaml:"http_referer"` // OpenRouter ranking header (optional)
	XTitle                    string `yaml:"x_title"`      // OpenRouter ranking header (optional)
}

type ServerConfig struct {
	GRPCAddr string   `yaml:"grpc_addr"`
	HTTPAddr string   `yaml:"http_addr"`
	DNSNames []string `yaml:"dns_names"`
	IPs      []string `yaml:"ip_addrs"`
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
	if cfg.LLM.BaseURL == "" {
		cfg.LLM.BaseURL = envOr("RECON_LLM_BASE_URL", "https://openrouter.ai/api/v1")
	}
	if cfg.LLM.Model == "" {
		cfg.LLM.Model = envOr("RECON_LLM_MODEL", "anthropic/claude-sonnet-4.5")
	}
	if cfg.LLM.APIKeyEnv == "" {
		cfg.LLM.APIKeyEnv = "RECON_LLM_API_KEY"
	}
	if cfg.LLM.MaxStepsPerInvestigation == 0 {
		cfg.LLM.MaxStepsPerInvestigation = 40
	}
	if cfg.LLM.MaxTokensPerInvestigation == 0 {
		cfg.LLM.MaxTokensPerInvestigation = 500_000
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
	return cfg, nil
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func (c *Config) ParsedIPs() []net.IP {
	out := make([]net.IP, 0, len(c.Server.IPs))
	for _, s := range c.Server.IPs {
		if ip := net.ParseIP(s); ip != nil {
			out = append(out, ip)
		}
	}
	return out
}
