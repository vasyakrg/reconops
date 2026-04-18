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
