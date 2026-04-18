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
	return cfg, nil
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
