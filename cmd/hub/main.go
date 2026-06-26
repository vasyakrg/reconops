// recon-hub is the central server: gRPC for agents (mTLS), HTTP for the
// operator UI, SQLite for state. See PROJECT.md §4.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/vasyakrg/recon/internal/common/logging"
	"github.com/vasyakrg/recon/internal/common/version"
	"github.com/vasyakrg/recon/internal/hub/api"
	"github.com/vasyakrg/recon/internal/hub/auth"
	"github.com/vasyakrg/recon/internal/hub/investigator"
	"github.com/vasyakrg/recon/internal/hub/llm"
	"github.com/vasyakrg/recon/internal/hub/release"
	"github.com/vasyakrg/recon/internal/hub/retention"
	hubrunner "github.com/vasyakrg/recon/internal/hub/runner"
	"github.com/vasyakrg/recon/internal/hub/store"
	"github.com/vasyakrg/recon/internal/hub/web"
)

func main() {
	cfgPath := flag.String("config", "/etc/recon/hub.yaml", "path to hub config")
	mode := flag.String("mode", "serve", "serve | gen-token | revoke | gen-password-hash")
	tokenTTL := flag.Duration("token-ttl", 24*time.Hour, "TTL for gen-token mode")
	tokenIssuer := flag.String("token-issued-by", "admin", "actor recorded for issued token")
	agentID := flag.String("agent-id", "", "target agent_id (required for gen-token / revoke)")
	revokeReason := flag.String("revoke-reason", "manual", "reason for revoke mode")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()
	if *showVersion {
		fmt.Println(version.Full())
		return
	}

	log, logLevel := logging.New()
	log.Info("recon-hub starting", "version", version.Full(), "mode", *mode, "config", *cfgPath,
		"log_level", logging.LevelString(logLevel))
	if raw := os.Getenv(logging.EnvLogLevel); raw != "" {
		if _, ok := logging.ParseLevel(raw); !ok {
			log.Warn("unrecognized RECON_LOG_LEVEL, defaulting to info",
				"value", raw, "accepted", "debug|info|warn|error")
		}
	}

	// gen-password-hash is a pure helper — runs before config / store / PKI
	// so the operator can produce a hash on a freshly installed binary.
	if *mode == "gen-password-hash" {
		pw := os.Getenv("RECON_ADMIN_PASSWORD")
		if pw == "" {
			fmt.Fprintln(os.Stderr, "set RECON_ADMIN_PASSWORD before invoking gen-password-hash")
			os.Exit(2)
		}
		h, err := web.GenPasswordHash(pw)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		fmt.Println(h)
		return
	}

	cfg, err := LoadConfig(*cfgPath)
	if err != nil {
		log.Error("load config", "err", err)
		os.Exit(2)
	}
	if err := os.MkdirAll(filepath.Dir(cfg.Storage.DBPath), 0o750); err != nil {
		log.Error("mkdir db dir", "err", err)
		os.Exit(2)
	}
	if err := os.MkdirAll(cfg.Storage.ArtifactDir, 0o750); err != nil {
		log.Error("mkdir artifact dir", "err", err)
		os.Exit(2)
	}
	serverIPs, err := cfg.ParsedIPs()
	if err != nil {
		log.Error("parse server ip_addrs", "err", err, "source", cfg.ServerIPSource())
		os.Exit(2)
	}
	llmScheme, llmHost := summarizeURLForLog(cfg.LLM.BaseURL)
	log.Info("resolved hub config",
		"llm_model", cfg.LLM.Model,
		"llm_base_url_scheme", llmScheme,
		"llm_base_url_host", llmHost,
		"llm_allow_insecure_http", cfg.LLM.AllowInsecureHTTP,
		"server_ip_san_source", cfg.ServerIPSource(),
		"server_ip_san_count", len(serverIPs),
	)
	if llmScheme == "http" && cfg.LLM.AllowInsecureHTTP {
		log.Warn("LLM plaintext HTTP is explicitly enabled for private/link-local router endpoints",
			"llm_base_url_host", llmHost)
	}
	// Surface an outbound LLM proxy so a hung/blocked provider call is not a
	// silent mystery. Log type + addr only — the proxy user/pass live in their
	// own env vars and never reach the log.
	if proxyType := strings.TrimSpace(os.Getenv("RECON_LLM_PROXY_TYPE")); proxyType != "" {
		log.Info("LLM outbound traffic routed through proxy",
			"proxy_type", proxyType,
			"proxy_addr", strings.TrimSpace(os.Getenv("RECON_LLM_PROXY_ADDR")))
	}
	for _, profile := range cfg.LLM.Profiles {
		profileScheme, profileHost := summarizeURLForLog(profile.BaseURL)
		log.Info("resolved llm profile",
			"profile", profile.Name,
			"role", profile.Role,
			"model", profile.Model,
			"base_url_scheme", profileScheme,
			"base_url_host", profileHost,
			"context_window_tokens", profile.ContextWindowTokens,
			"max_output_tokens", profile.MaxOutputTokens,
			"supports_tools", profile.SupportsTools,
			"supports_prompt_cache", profile.SupportsPromptCache)
		if profile.ContextWindowFallback {
			log.Warn("llm profile uses fallback context window",
				"profile", profile.Name,
				"role", profile.Role,
				"model", profile.Model,
				"context_window_tokens", profile.ContextWindowTokens)
		}
	}

	rootCtx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	st, err := store.Open(rootCtx, cfg.Storage.DBPath)
	if err != nil {
		log.Error("open store", "err", err)
		os.Exit(2)
	}
	defer func() { _ = st.Close() }()

	pki, err := auth.Bootstrap(cfg.Storage.CADir, cfg.Server.DNSNames, serverIPs)
	if err != nil {
		log.Error("bootstrap PKI", "err", err)
		os.Exit(2)
	}

	switch *mode {
	case "gen-token":
		if *agentID == "" {
			log.Error("--agent-id is required for gen-token (token is bound to one agent)")
			os.Exit(2)
		}
		token, err := auth.GenerateBootstrapToken()
		if err != nil {
			log.Error("gen-token", "err", err)
			os.Exit(2)
		}
		if err := st.InsertBootstrapToken(rootCtx, token, *agentID, *tokenIssuer, *tokenTTL); err != nil {
			log.Error("store token", "err", err)
			os.Exit(2)
		}
		_ = st.AuditLog(rootCtx, *tokenIssuer, "token.issue", map[string]any{"agent_id": *agentID, "ttl": tokenTTL.String()})
		fmt.Println(token)
		return

	case "revoke":
		if *agentID == "" {
			log.Error("--agent-id is required for revoke")
			os.Exit(2)
		}
		if err := st.RevokeIdentity(rootCtx, *agentID, *revokeReason); err != nil {
			log.Error("revoke", "err", err)
			os.Exit(2)
		}
		_ = st.AuditLog(rootCtx, *tokenIssuer, "identity.revoke", map[string]any{"agent_id": *agentID, "reason": *revokeReason})
		log.Info("identity revoked", "agent_id", *agentID)
		return

	case "serve":
		// continue below

	default:
		log.Error("unknown mode", "mode", *mode)
		os.Exit(2)
	}

	apiSrv := api.NewServer(st, pki, log.With("comp", "grpc"))
	hr := hubrunner.New(st, apiSrv, cfg.Storage.ArtifactDir, cfg.Runner.PerAgentRPM, log.With("comp", "runner"))

	// Retention worker: housekeeping artifacts + archived messages.
	rw := retention.New(st, cfg.Storage.ArtifactDir, cfg.Storage.RetentionDays, time.Hour, log.With("comp", "retention"))
	go rw.Run(rootCtx)
	apiSrv.SetSink(hr)

	// LLM client is optional — if no API key is configured, the
	// investigator endpoints will return a clear startup-time error when
	// invoked, but the hub still serves /hosts/{id} + /runs.
	var loop *investigator.Loop
	availability := web.NewInvestigatorAvailability(nil, "")
	primaryProfile := cfg.LLM.PrimaryProfile()
	llmClient, llmErr := llm.NewFromEnv(primaryProfile.BaseURL, primaryProfile.Model, primaryProfile.APIKeyEnv,
		primaryProfile.AllowInsecureHTTP, primaryProfile.HTTPReferer, primaryProfile.XTitle)
	if llmErr != nil {
		availability = web.NewInvestigatorAvailability(nil, llmErr.Error())
		log.Warn("LLM client disabled (investigator endpoints will refuse)",
			"reason_class", availability.DisabledReason,
			"llm_profile", primaryProfile.Name,
			"llm_model", primaryProfile.Model,
			"llm_base_url_scheme", llmScheme,
			"llm_base_url_host", llmHost,
			"llm_api_key_env", primaryProfile.APIKeyEnv)
	} else {
		log.Info("LLM client ready",
			"llm_profile", primaryProfile.Name,
			"llm_model", llmClient.Model(),
			"llm_base_url_scheme", llmScheme,
			"llm_base_url_host", llmHost,
			"llm_request_timeout", llmClient.Timeout())
		// Best-effort: learn the real context window from GET /models when the
		// operator left context_window_tokens unset (Fix B). Never overrides an
		// explicit config value; a miss keeps the conservative fallback.
		maybeAutodetectContextWindow(rootCtx, cfg, &primaryProfile, llmClient, log)
		loop = investigator.NewLoop(st, llmClient, hr, apiSrv.IsOnline, apiSrv.OnlineAgents,
			cfg.LLM.MaxStepsPerInvestigation, cfg.LLM.MaxTokensPerInvestigation,
			log.With("comp", "investigator"))
		loop.SetContextLimits(primaryProfile.ContextWindowTokens, primaryProfile.MaxOutputTokens)
		loop.SetMaxResultTokens(cfg.LLM.MaxResultTokens)
		loop.SetHistoryDemotion(cfg.LLM.HistoryKeepRecentResults, cfg.LLM.HistoryDemoteMinBytes)
		if cfg.Investigator.RerankIntervalSteps != nil {
			loop.SetRerankInterval(*cfg.Investigator.RerankIntervalSteps)
		}
		loop.SetArtifactDir(cfg.Storage.ArtifactDir)
		// Per-operation model routing (Task 13). Build one client per profile;
		// on any construction error (e.g. a secondary profile's key env unset)
		// fall back to the single primary client already wired into the loop.
		if router, rerr := llm.NewRouter(llmRouterProfiles(cfg.LLM.Profiles)); rerr != nil {
			log.Warn("model router disabled — using single primary client", "err", rerr)
		} else {
			loop.SetRouter(router)
			log.Info("model router ready", "profiles", len(cfg.LLM.Profiles))
		}
		availability = web.NewInvestigatorAvailability(loop, "")
		loop.SetBus(investigator.NewBus())
		// Cross-investigation priors: inject a compact, host-scoped digest of
		// prior done investigations into each new run. Defaults are compiled in;
		// investigator.priors.* in hub.yaml overrides (unset keys keep defaults).
		priors := investigator.DefaultPriorsConfig()
		pc := cfg.Investigator.Priors
		if pc.Enabled != nil {
			priors.Enabled = *pc.Enabled
		}
		if pc.MaxInvestigations > 0 {
			priors.MaxInvestigations = pc.MaxInvestigations
		}
		if pc.MaxFindingsPerInvestigation > 0 {
			priors.MaxFindingsPerInv = pc.MaxFindingsPerInvestigation
		}
		if pc.Scope != "" {
			priors.Scope = pc.Scope
		}
		if pc.MaxAgeDays > 0 {
			priors.MaxAgeDays = pc.MaxAgeDays
		}
		loop.SetPriorsConfig(priors)
		// Resume investigations that were active before this hub restarted —
		// their loop goroutines died with the previous process.
		if err := loop.Resume(rootCtx); err != nil {
			log.Warn("investigator resume", "err", err)
		}
	}

	lis, gsrv, err := apiSrv.Listen(cfg.Server.GRPCAddr)
	if err != nil {
		log.Error("grpc listen", "err", err)
		os.Exit(2)
	}
	go func() {
		log.Info("grpc listening", "addr", cfg.Server.GRPCAddr)
		if err := gsrv.Serve(lis); err != nil {
			log.Error("grpc serve", "err", err)
			cancel()
		}
	}()
	go func() {
		<-rootCtx.Done()
		gsrv.GracefulStop()
	}()

	auth := web.AuthConfig{
		Username:       envOr("RECON_ADMIN_USER", ""),
		PasswordHash:   envOr("RECON_ADMIN_PASSWORD_HASH", ""),
		BehindTLSProxy: envOr("RECON_BEHIND_TLS_PROXY", "") == "true",
	}
	// Convenience: if the operator passes the plaintext password directly,
	// hash it here at startup so they don't have to run gen-password-hash
	// as a separate step. RECON_ADMIN_PASSWORD_HASH still wins when both
	// are set (useful for handing out a hash without ever exposing the
	// plaintext to whoever maintains the env file). bcrypt cost is ~100ms,
	// paid once at boot — fine.
	if auth.PasswordHash == "" {
		if pw := os.Getenv("RECON_ADMIN_PASSWORD"); pw != "" {
			h, err := web.GenPasswordHash(pw)
			if err != nil {
				log.Error("hash RECON_ADMIN_PASSWORD", "err", err)
				os.Exit(2)
			}
			auth.PasswordHash = h
			log.Info("hashed RECON_ADMIN_PASSWORD on startup", "user", auth.Username)
		}
	}
	if auth.Username != "" && auth.PasswordHash == "" {
		log.Error("RECON_ADMIN_USER set but neither RECON_ADMIN_PASSWORD nor RECON_ADMIN_PASSWORD_HASH — refusing to start")
		os.Exit(2)
	}
	if !auth.Enabled() {
		log.Warn("hub is running WITHOUT auth — bind to loopback only and reverse-proxy with auth before exposing")
	}

	install := web.InstallConfig{
		ReleaseRepoURL:    cfg.Install.ReleaseRepoURL,
		AgentGRPCEndpoint: cfg.Install.AgentGRPCEndpoint,
		GRPCPort:          cfg.Install.GRPCPort,
		Version:           cfg.Install.Version,
		ExternalURL:       cfg.Install.ExternalURL,
		TrustedTLS:        cfg.Install.TrustedTLS,
		SelfHosted:        cfg.Install.SelfHosted,
		ReleasesDir:       cfg.Install.ReleasesDir,
	}
	// Release poller — the "latest agent version" source for the outdated UI.
	// Self-hosted mode (SH4): latest = the agent version baked into this hub
	// image (version.Version), no api.github.com round-trip. GitHub mode:
	// best-effort poll of the configured repo's Releases API; nil when the repo
	// URL isn't a GitHub https:// URL, in which case the UI degrades silently.
	var relPoll *release.Poller
	if cfg.Install.SelfHosted {
		relPoll = release.NewStatic(version.Version)
		log.Info("self-hosted release source", "latest_agent", version.Version)
	} else {
		relPoll = release.New(cfg.Install.ReleaseRepoURL, 0, log.With("comp", "release"))
	}
	if relPoll != nil {
		go relPoll.Run(rootCtx)
	}
	webSrv, err := web.NewServer(st, hr, loop, availability, relPoll, auth, install, log.With("comp", "web"))
	if err != nil {
		log.Error("web init", "err", err)
		os.Exit(2)
	}
	webSrv.SetLLMProfiles(llmProfileViews(cfg.LLM.Profiles))
	webSrv.SetArtifactDir(cfg.Storage.ArtifactDir)
	certFile, keyFile := "", ""
	if cfg.Server.HTTPTLS.Enabled {
		certFile = cfg.Server.HTTPTLS.CertFile
		keyFile = cfg.Server.HTTPTLS.KeyFile
		if certFile == "" || keyFile == "" {
			log.Error("server.http_tls.enabled=true but cert_file/key_file missing")
			os.Exit(2)
		}
	}
	if err := webSrv.ServeTLS(rootCtx, cfg.Server.HTTPAddr, certFile, keyFile); err != nil {
		log.Error("web serve", "err", err)
	}
}

func llmProfileViews(profiles []LLMProfileConfig) []web.LLMProfileView {
	out := make([]web.LLMProfileView, 0, len(profiles))
	for _, profile := range profiles {
		out = append(out, web.LLMProfileView{
			Name:                  profile.Name,
			Role:                  profile.Role,
			Model:                 profile.Model,
			BaseURL:               profile.BaseURL,
			ContextWindowTokens:   profile.ContextWindowTokens,
			MaxOutputTokens:       profile.MaxOutputTokens,
			SupportsTools:         profile.SupportsTools,
			SupportsPromptCache:   profile.SupportsPromptCache,
			ContextWindowFallback: profile.ContextWindowFallback,
		})
	}
	return out
}

// llmRouterProfiles maps the resolved hub config profiles onto the llm
// package's routing-facing Profile so the router can build a client per
// profile without the llm package importing package main.
func llmRouterProfiles(profiles []LLMProfileConfig) []llm.Profile {
	out := make([]llm.Profile, 0, len(profiles))
	for _, p := range profiles {
		out = append(out, llm.Profile{
			Name:                p.Name,
			Role:                p.Role,
			Model:               p.Model,
			BaseURL:             p.BaseURL,
			APIKeyEnv:           p.APIKeyEnv,
			ContextWindowTokens: p.ContextWindowTokens,
			MaxOutputTokens:     p.MaxOutputTokens,
			SupportsTools:       p.SupportsTools,
			SupportsPromptCache: p.SupportsPromptCache,
			AllowInsecureHTTP:   p.AllowInsecureHTTP,
			HTTPReferer:         p.HTTPReferer,
			XTitle:              p.XTitle,
		})
	}
	return out
}

func summarizeURLForLog(raw string) (scheme, host string) {
	u, err := url.Parse(raw)
	if err != nil {
		return "invalid", ""
	}
	return u.Scheme, u.Host
}

// maybeAutodetectContextWindow probes GET /models once and fills the primary
// profile's context window when the operator left it unset (ContextWindowFallback
// is true). It updates both the local primaryProfile (used for SetContextLimits)
// and the matching cfg.LLM.Profiles entry (router + /settings view), clearing the
// fallback flag so diagnostics stop flagging it as a guess. Any probe failure or
// an absent window is a soft miss that keeps the conservative fallback — and an
// operator-set context_window_tokens is never overridden.
func maybeAutodetectContextWindow(ctx context.Context, cfg *Config, primary *LLMProfileConfig, client *llm.Client, log *slog.Logger) {
	if !cfg.LLM.AutodetectContextWindowEnabled() || !primary.ContextWindowFallback {
		return
	}
	probeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	windows, err := client.ListModels(probeCtx)
	if err != nil {
		log.Debug("context window auto-detect skipped (provider /models probe failed)",
			"llm_profile", primary.Name, "model", primary.Model, "err", err)
		return
	}
	w, ok := windows[primary.Model]
	if !ok || w <= 0 {
		log.Debug("context window not exposed by provider /models; keeping fallback",
			"llm_profile", primary.Name, "model", primary.Model,
			"fallback_context_window_tokens", primary.ContextWindowTokens)
		return
	}
	log.Info("context window auto-detected from provider /models",
		"llm_profile", primary.Name, "model", primary.Model,
		"detected_context_window_tokens", w, "previous_fallback_tokens", primary.ContextWindowTokens)
	primary.ContextWindowTokens = w
	primary.ContextWindowFallback = false
	for i := range cfg.LLM.Profiles {
		if cfg.LLM.Profiles[i].Name == primary.Name {
			cfg.LLM.Profiles[i].ContextWindowTokens = w
			cfg.LLM.Profiles[i].ContextWindowFallback = false
		}
	}
}
