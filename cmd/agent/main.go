// recon-agent is the read-only diagnostic agent. See PROJECT.md §3.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/vasyakrg/recon/internal/agent/collect"
	_ "github.com/vasyakrg/recon/internal/agent/collectors/container" // register docker_*
	_ "github.com/vasyakrg/recon/internal/agent/collectors/files"     // register file_read, disk_usage
	_ "github.com/vasyakrg/recon/internal/agent/collectors/k8s"       // register kubectl_*
	_ "github.com/vasyakrg/recon/internal/agent/collectors/net"       // register net_*
	_ "github.com/vasyakrg/recon/internal/agent/collectors/process"   // register process_list
	_ "github.com/vasyakrg/recon/internal/agent/collectors/system"    // register system_info
	_ "github.com/vasyakrg/recon/internal/agent/collectors/systemd"   // register systemd_units, journal_tail
	"github.com/vasyakrg/recon/internal/agent/conn"
	"github.com/vasyakrg/recon/internal/agent/exec"
	"github.com/vasyakrg/recon/internal/agent/update"
	"github.com/vasyakrg/recon/internal/common/version"
)

func main() {
	cfgPath := flag.String("config", "/etc/recon/agent.yaml", "path to agent config")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()
	if *showVersion {
		fmt.Println(version.Full())
		return
	}

	log := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	log.Info("recon-agent starting", "version", version.Full(), "config", *cfgPath)

	// Register the exec gateway whitelist before any collector can run.
	// Collectors that try to invoke binaries outside this list will panic
	// (PROJECT.md §3.4 layer 3); the agent runner recovers and reports
	// STATUS_ERROR — agent stays up.
	exec.RegisterDefaults()

	// Initial capability probe: marks every Availabler-implementing
	// collector as available/unavailable per current host state (binary
	// present on disk, etc). Collectors not implementing Availabler are
	// always visible. The agent's connect loop then re-probes on a 60s
	// timer and re-sends Hello on any diff, so installing docker or
	// systemctl mid-session brings the corresponding collectors online
	// without an agent restart (see capabilityProbeLoop).
	if diff := collect.RefreshAvailability(); len(diff.NowUnavailable) > 0 {
		log.Info("collectors registered but unavailable on this host", "names", diff.NowUnavailable)
	}

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

	// Opt-in self-updater (off by default — see update.UpdateConfig docs).
	if cfg.Update.Enabled {
		upd := update.New(update.Options{
			RepoURL:         cfg.Update.RepoURL,
			CheckInterval:   cfg.Update.CheckInterval,
			BinaryPath:      cfg.Update.BinaryPath,
			AllowPrerelease: cfg.Update.AllowPrerelease,
		}, log.With("comp", "selfupdate"))
		if upd != nil {
			go upd.Run(ctx)
		} else {
			log.Warn("self-updater enabled in config but disabled — repo_url/binary_path missing or invalid")
		}
	}

	c := conn.NewClient(cfg, log)
	if err := c.Run(ctx); err != nil && err != context.Canceled {
		log.Error("agent run", "err", err)
		os.Exit(1)
	}
}
