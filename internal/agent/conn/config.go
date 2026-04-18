package conn

import (
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Hub      HubConfig      `yaml:"hub"`
	Identity IdentityConfig `yaml:"identity"`
	Runtime  RuntimeConfig  `yaml:"runtime"`
}

type HubConfig struct {
	Endpoint             string `yaml:"endpoint"`
	CACert               string `yaml:"ca_cert"`
	Cert                 string `yaml:"cert"`
	Key                  string `yaml:"key"`
	BootstrapToken       string `yaml:"bootstrap_token,omitempty"` // file path
	BootstrapTokenInline string `yaml:"bootstrap_token_inline,omitempty"`
	ServerName           string `yaml:"server_name,omitempty"` // SNI override
}

type IdentityConfig struct {
	ID     string            `yaml:"id"`
	Labels map[string]string `yaml:"labels"`
}

type RuntimeConfig struct {
	MaxConcurrentCollectors int           `yaml:"max_concurrent_collectors"`
	ArtifactDir             string        `yaml:"artifact_dir"`
	DefaultTimeout          time.Duration `yaml:"default_timeout"`
	HeartbeatInterval       time.Duration `yaml:"heartbeat_interval"`
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
	if cfg.Hub.Endpoint == "" {
		return nil, fmt.Errorf("hub.endpoint required")
	}
	if cfg.Identity.ID == "" {
		host, _ := os.Hostname()
		cfg.Identity.ID = host
	}
	if cfg.Runtime.HeartbeatInterval == 0 {
		cfg.Runtime.HeartbeatInterval = 15 * time.Second
	}
	if cfg.Runtime.DefaultTimeout == 0 {
		cfg.Runtime.DefaultTimeout = 30 * time.Second
	}
	if cfg.Runtime.MaxConcurrentCollectors == 0 {
		cfg.Runtime.MaxConcurrentCollectors = 4
	}
	if cfg.Identity.Labels == nil {
		cfg.Identity.Labels = map[string]string{}
	}
	return cfg, nil
}
