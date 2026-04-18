// recon-agent is the read-only diagnostic agent. See PROJECT.md §3.
package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	_ "github.com/vasyakrg/recon/internal/agent/collectors/system" // register system_info
	"github.com/vasyakrg/recon/internal/agent/conn"
	"github.com/vasyakrg/recon/internal/common/version"
)

func main() {
	cfgPath := flag.String("config", "/etc/recon/agent.yaml", "path to agent config")
	flag.Parse()

	log := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	log.Info("recon-agent starting", "version", version.Full(), "config", *cfgPath)

	cfg, err := conn.LoadConfig(*cfgPath)
	if err != nil {
		log.Error("load config", "err", err)
		os.Exit(2)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	if err := conn.Enroll(ctx, cfg); err != nil {
		log.Error("enroll", "err", err)
		os.Exit(3)
	}

	c := conn.NewClient(cfg, log)
	if err := c.Run(ctx); err != nil && err != context.Canceled {
		log.Error("agent run", "err", err)
		os.Exit(1)
	}
}
