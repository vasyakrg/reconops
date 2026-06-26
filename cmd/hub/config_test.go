package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadConfigLLMEnvOverridesYAML(t *testing.T) {
	t.Setenv(envLLMModel, "env/model")
	t.Setenv(envLLMBaseURL, "https://env.example/v1")

	cfg := loadConfigFixture(t, `
llm:
  model: yaml/model
  base_url: https://yaml.example/v1
`)

	if cfg.LLM.Model != "env/model" {
		t.Fatalf("model=%q", cfg.LLM.Model)
	}
	if cfg.LLM.BaseURL != "https://env.example/v1" {
		t.Fatalf("base_url=%q", cfg.LLM.BaseURL)
	}
}

func TestLoadConfigUnsetLLMEnvPreservesYAML(t *testing.T) {
	cfg := loadConfigFixture(t, `
llm:
  model: yaml/model
  base_url: https://yaml.example/v1
`)

	if cfg.LLM.Model != "yaml/model" {
		t.Fatalf("model=%q", cfg.LLM.Model)
	}
	if cfg.LLM.BaseURL != "https://yaml.example/v1" {
		t.Fatalf("base_url=%q", cfg.LLM.BaseURL)
	}
}

func TestLoadConfigUsesDefaultLLMValuesWhenUnset(t *testing.T) {
	cfg := loadConfigFixture(t, `{}`)

	if cfg.LLM.Model != defaultLLMName {
		t.Fatalf("model=%q", cfg.LLM.Model)
	}
	if cfg.LLM.BaseURL != defaultLLMURL {
		t.Fatalf("base_url=%q", cfg.LLM.BaseURL)
	}
	if len(cfg.LLM.Profiles) != 1 {
		t.Fatalf("profiles=%+v", cfg.LLM.Profiles)
	}
	primary := cfg.LLM.PrimaryProfile()
	if primary.Role != "primary" || primary.Model != defaultLLMName || !primary.SupportsTools {
		t.Fatalf("primary profile not resolved from legacy defaults: %+v", primary)
	}
	if primary.ContextWindowTokens != 200000 || !primary.ContextWindowFallback {
		t.Fatalf("default context fallback wrong: %+v", primary)
	}
	if !cfg.LLM.AutodetectContextWindowEnabled() {
		t.Fatalf("autodetect_context_window should default ON when unset")
	}
}

func TestAutodetectContextWindowExplicitDisable(t *testing.T) {
	cfg := loadConfigFixture(t, `
llm:
  autodetect_context_window: false
`)
	if cfg.LLM.AutodetectContextWindowEnabled() {
		t.Fatalf("autodetect_context_window: false must disable the probe")
	}
}

func TestLoadConfigProfilesPreserveEnvOverrideForPrimary(t *testing.T) {
	t.Setenv(envLLMModel, "env/model")
	t.Setenv(envLLMBaseURL, "https://env.example/v1")

	cfg := loadConfigFixture(t, `
llm:
  api_key_env: YAML_KEY
  profiles:
    - name: cheap
      role: cheap
      model: cheap/model
      base_url: https://cheap.example/v1
      api_key_env: CHEAP_KEY
      context_window_tokens: 64000
      max_output_tokens: 1024
`)

	if len(cfg.LLM.Profiles) != 2 {
		t.Fatalf("profiles=%+v", cfg.LLM.Profiles)
	}
	primary := cfg.LLM.PrimaryProfile()
	if primary.Name != "primary" || primary.Model != "env/model" || primary.BaseURL != "https://env.example/v1" {
		t.Fatalf("primary did not inherit env-overridden legacy fields: %+v", primary)
	}
	if cfg.LLM.Profiles[1].Name != "cheap" || cfg.LLM.Profiles[1].ContextWindowTokens != 64000 {
		t.Fatalf("cheap profile not preserved: %+v", cfg.LLM.Profiles[1])
	}
}

func TestLoadConfigExplicitPrimaryProfile(t *testing.T) {
	cfg := loadConfigFixture(t, `
llm:
  model: yaml/model
  base_url: https://yaml.example/v1
  profiles:
    - name: main
      role: primary
      model: profile/model
      base_url: https://profile.example/v1
      api_key_env: PROFILE_KEY
      context_window_tokens: 256000
      max_output_tokens: 8192
      supports_tools: true
      supports_prompt_cache: true
`)

	if len(cfg.LLM.Profiles) != 1 {
		t.Fatalf("profiles=%+v", cfg.LLM.Profiles)
	}
	primary := cfg.LLM.PrimaryProfile()
	if cfg.LLM.Model != "profile/model" || cfg.LLM.BaseURL != "https://profile.example/v1" || cfg.LLM.APIKeyEnv != "PROFILE_KEY" {
		t.Fatalf("legacy active fields not aligned to primary profile: cfg=%+v primary=%+v", cfg.LLM, primary)
	}
	if primary.ContextWindowTokens != 256000 || primary.ContextWindowFallback {
		t.Fatalf("explicit context not preserved: %+v", primary)
	}
	if !primary.SupportsPromptCache || !primary.SupportsTools {
		t.Fatalf("capabilities not preserved: %+v", primary)
	}
}

func TestLoadConfigRejectsUnsupportedProfileRole(t *testing.T) {
	_, err := LoadConfig(writeConfigFixture(t, `
llm:
  profiles:
    - name: odd
      role: poet
      model: m
      base_url: https://example.test/v1
`))
	if err == nil {
		t.Fatal("expected unsupported role error")
	}
	if !strings.Contains(err.Error(), "unsupported role") || !strings.Contains(err.Error(), "poet") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestLoadConfigHubIPsFromEnv(t *testing.T) {
	t.Setenv(envHubIPAddrs, " 192.168.1.10, ,10.0.0.2 ")

	cfg := loadConfigFixture(t, `
server:
  ip_addrs: ["127.0.0.1"]
`)

	want := []string{"192.168.1.10", "10.0.0.2"}
	if strings.Join(cfg.Server.IPs, ",") != strings.Join(want, ",") {
		t.Fatalf("Server.IPs=%v", cfg.Server.IPs)
	}
	if cfg.ServerIPSource() != "env:"+envHubIPAddrs {
		t.Fatalf("ServerIPSource=%q", cfg.ServerIPSource())
	}
	parsed, err := cfg.ParsedIPs()
	if err != nil {
		t.Fatal(err)
	}
	if len(parsed) != 2 {
		t.Fatalf("parsed IP count=%d", len(parsed))
	}
}

func TestLoadConfigAllowInsecureHTTPEnvOverridesYAML(t *testing.T) {
	t.Setenv(envLLMAllowInsecureHTTP, "false")

	cfg := loadConfigFixture(t, `
llm:
  allow_insecure_http: true
`)

	if cfg.LLM.AllowInsecureHTTP {
		t.Fatal("expected env false to override YAML true")
	}

	t.Setenv(envLLMAllowInsecureHTTP, "true")
	cfg = loadConfigFixture(t, `{}`)
	if !cfg.LLM.AllowInsecureHTTP {
		t.Fatal("expected env true to enable allow_insecure_http")
	}
}

func TestLoadConfigRejectsMalformedAllowInsecureHTTPEnv(t *testing.T) {
	t.Setenv(envLLMAllowInsecureHTTP, "sometimes")

	_, err := LoadConfig(writeConfigFixture(t, `{}`))
	if err == nil {
		t.Fatal("expected malformed bool env error")
	}
	if !strings.Contains(err.Error(), envLLMAllowInsecureHTTP) || !strings.Contains(err.Error(), "sometimes") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestLoadConfigRejectsMalformedHubIPEnv(t *testing.T) {
	t.Setenv(envHubIPAddrs, "192.168.1.10, not-an-ip")

	_, err := LoadConfig(writeConfigFixture(t, `{}`))
	if err == nil {
		t.Fatal("expected malformed env IP error")
	}
	if !strings.Contains(err.Error(), envHubIPAddrs) || !strings.Contains(err.Error(), "not-an-ip") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestLoadConfigRejectsMalformedYAMLIP(t *testing.T) {
	_, err := LoadConfig(writeConfigFixture(t, `
server:
  ip_addrs: ["127.0.0.1", "bad-ip"]
`))
	if err == nil {
		t.Fatal("expected malformed YAML IP error")
	}
	if !strings.Contains(err.Error(), "server.ip_addrs") || !strings.Contains(err.Error(), "bad-ip") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func loadConfigFixture(t *testing.T, body string) *Config {
	t.Helper()
	cfg, err := LoadConfig(writeConfigFixture(t, body))
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func writeConfigFixture(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "hub.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}
