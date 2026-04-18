// recon-hub is the central server: gRPC for agents (mTLS), HTTP for the
// operator UI, SQLite for state. See PROJECT.md §4.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/vasyakrg/recon/internal/common/version"
	"github.com/vasyakrg/recon/internal/hub/api"
	"github.com/vasyakrg/recon/internal/hub/auth"
	"github.com/vasyakrg/recon/internal/hub/investigator"
	"github.com/vasyakrg/recon/internal/hub/llm"
	hubrunner "github.com/vasyakrg/recon/internal/hub/runner"
	"github.com/vasyakrg/recon/internal/hub/store"
	"github.com/vasyakrg/recon/internal/hub/web"
)

func main() {
	cfgPath := flag.String("config", "/etc/recon/hub.yaml", "path to hub config")
	mode := flag.String("mode", "serve", "serve | gen-token | revoke")
	tokenTTL := flag.Duration("token-ttl", 24*time.Hour, "TTL for gen-token mode")
	tokenIssuer := flag.String("token-issued-by", "admin", "actor recorded for issued token")
	agentID := flag.String("agent-id", "", "target agent_id (required for gen-token / revoke)")
	revokeReason := flag.String("revoke-reason", "manual", "reason for revoke mode")
	flag.Parse()

	log := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	log.Info("recon-hub starting", "version", version.Full(), "mode", *mode, "config", *cfgPath)

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

	rootCtx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	st, err := store.Open(rootCtx, cfg.Storage.DBPath)
	if err != nil {
		log.Error("open store", "err", err)
		os.Exit(2)
	}
	defer func() { _ = st.Close() }()

	pki, err := auth.Bootstrap(cfg.Storage.CADir, cfg.Server.DNSNames, cfg.ParsedIPs())
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
	hr := hubrunner.New(st, apiSrv, cfg.Storage.ArtifactDir, log.With("comp", "runner"))
	apiSrv.SetSink(hr)

	// LLM client is optional — if no API key is configured, the
	// investigator endpoints will return a clear startup-time error when
	// invoked, but the hub still serves /hosts/{id} + /runs.
	var loop *investigator.Loop
	llmClient, llmErr := llm.NewFromEnv(cfg.LLM.BaseURL, cfg.LLM.Model, cfg.LLM.APIKeyEnv, cfg.LLM.HTTPReferer, cfg.LLM.XTitle)
	if llmErr != nil {
		log.Warn("LLM client disabled (investigator endpoints will refuse)", "err", llmErr,
			"model", cfg.LLM.Model, "base_url", cfg.LLM.BaseURL, "api_key_env", cfg.LLM.APIKeyEnv)
	} else {
		log.Info("LLM client ready", "model", llmClient.Model(), "base_url", cfg.LLM.BaseURL)
		loop = investigator.NewLoop(st, llmClient, hr, apiSrv.IsOnline, apiSrv.OnlineAgents,
			cfg.LLM.MaxStepsPerInvestigation, cfg.LLM.MaxTokensPerInvestigation,
			log.With("comp", "investigator"))
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

	webSrv, err := web.NewServer(st, hr, loop, log.With("comp", "web"))
	if err != nil {
		log.Error("web init", "err", err)
		os.Exit(2)
	}
	if err := webSrv.Serve(rootCtx, cfg.Server.HTTPAddr); err != nil {
		log.Error("web serve", "err", err)
	}
}
