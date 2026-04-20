package conn

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net"
	"os"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/connectivity"
	"google.golang.org/grpc/credentials"

	"github.com/vasyakrg/recon/internal/agent/collect"
	"github.com/vasyakrg/recon/internal/agent/runner"
	"github.com/vasyakrg/recon/internal/common/version"
	reconpb "github.com/vasyakrg/recon/internal/proto"
)

// streamSender serializes Send() calls into the bidi gRPC stream. The runner
// emits results from goroutines, the heartbeat loop emits Heartbeats, and
// gRPC streams are not safe for concurrent Send.
type streamSender struct {
	mu     sync.Mutex
	stream reconpb.Hub_ConnectClient
}

func (s *streamSender) Send(m *reconpb.AgentMsg) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stream.Send(m)
}

type Client struct {
	cfg *Config
	log *slog.Logger
}

func NewClient(cfg *Config, log *slog.Logger) *Client {
	return &Client{cfg: cfg, log: log}
}

// Run drives the connection loop. It reconnects with bounded exponential
// backoff + jitter on every disconnect and never returns until ctx is done.
func (c *Client) Run(ctx context.Context) error {
	backoff := time.Second
	const maxBackoff = 30 * time.Second
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		err := c.session(ctx)
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return err
		}
		c.log.Warn("agent session ended", "err", err, "backoff", backoff)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(jitter(backoff)):
		}
		backoff *= 2
		if backoff > maxBackoff {
			backoff = maxBackoff
		}
	}
}

func jitter(d time.Duration) time.Duration {
	return d/2 + time.Duration(rand.Int64N(int64(d/2)+1)) //nolint:gosec
}

func (c *Client) session(ctx context.Context) error {
	creds, err := c.mTLSCreds()
	if err != nil {
		return fmt.Errorf("mtls: %w", err)
	}
	// Custom dialer pinned to tcp4. gRPC's default dialer happy-eyeballs
	// IPv6 first when the host has any v6 route configured, and we've seen
	// VMs return ENETUNREACH for the v6 attempt even though `nc -vz` over
	// v4 succeeds. Forcing tcp4 sidesteps that asymmetry — the hub
	// endpoint is always a v4 host:port in the install one-liner.
	dialer := func(ctx context.Context, addr string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "tcp4", addr)
	}
	conn, err := grpc.NewClient(c.cfg.Hub.Endpoint,
		grpc.WithTransportCredentials(creds),
		grpc.WithContextDialer(dialer),
	)
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	defer func() { _ = conn.Close() }()

	client := reconpb.NewHubClient(conn)
	stream, err := client.Connect(ctx)
	if err != nil {
		return fmt.Errorf("connect rpc: %w", err)
	}

	// Force the underlying TCP+TLS handshake before we declare success —
	// gRPC NewClient is non-blocking and Connect() returns a stream
	// before bytes hit the wire. WaitForStateChange with a 15s deadline
	// turns "agent appears active but logs nothing" (SAN mismatch, dropped
	// packets) into a clean error that triggers the backoff loop with a
	// visible "agent session ended" log line.
	connectCtx, cancelConnect := context.WithTimeout(ctx, 15*time.Second)
	defer cancelConnect()
	for state := conn.GetState(); state != connectivity.Ready; state = conn.GetState() {
		if !conn.WaitForStateChange(connectCtx, state) {
			return fmt.Errorf("dial timeout to %s: state=%s", c.cfg.Hub.Endpoint, state)
		}
		if state == connectivity.TransientFailure {
			conn.Connect()
		}
	}

	send := &streamSender{stream: stream}
	if err := c.sendHello(send); err != nil {
		return fmt.Errorf("hello: %w", err)
	}
	c.log.Info("hello sent", "agent_id", c.cfg.Identity.ID, "endpoint", c.cfg.Hub.Endpoint)

	run := runner.New(c.cfg.Runtime.MaxConcurrentCollectors, c.cfg.Runtime.DefaultTimeout, c.log.With("comp", "runner"))

	hbCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go c.heartbeatLoop(hbCtx, send)
	go c.capabilityProbeLoop(hbCtx, send)

	for {
		msg, err := stream.Recv()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		switch p := msg.Payload.(type) {
		case *reconpb.HubMsg_Collect:
			c.log.Info("collect request", "request_id", p.Collect.RequestId, "collector", p.Collect.Collector)
			run.Handle(hbCtx, p.Collect, send)
		case *reconpb.HubMsg_Cancel:
			ok := run.Cancel(p.Cancel.RequestId)
			c.log.Info("cancel request", "request_id", p.Cancel.RequestId, "found", ok)
		case *reconpb.HubMsg_Config:
			c.log.Info("config update", "values", p.Config.Values)
		}
	}
}

func (c *Client) mTLSCreds() (credentials.TransportCredentials, error) {
	cert, err := tls.LoadX509KeyPair(c.cfg.Hub.Cert, c.cfg.Hub.Key)
	if err != nil {
		return nil, fmt.Errorf("client keypair: %w", err)
	}
	caPEM, err := os.ReadFile(c.cfg.Hub.CACert)
	if err != nil {
		return nil, fmt.Errorf("ca cert: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, errors.New("ca cert: invalid PEM")
	}
	return credentials.NewTLS(&tls.Config{
		Certificates: []tls.Certificate{cert},
		RootCAs:      pool,
		ServerName:   c.serverName(),
		MinVersion:   tls.VersionTLS12,
	}), nil
}

func (c *Client) serverName() string {
	if c.cfg.Hub.ServerName != "" {
		return c.cfg.Hub.ServerName
	}
	host := c.cfg.Hub.Endpoint
	for i := len(host) - 1; i >= 0; i-- {
		if host[i] == ':' {
			host = host[:i]
			break
		}
	}
	return host
}

func (c *Client) sendHello(send *streamSender) error {
	manifests := collect.Manifests()
	pbManifests := make([]*reconpb.CollectorManifest, 0, len(manifests))
	for _, m := range manifests {
		body, _ := m.JSON()
		reqs := make([]string, 0, len(m.Requires))
		for _, r := range m.Requires {
			reqs = append(reqs, string(r))
		}
		pbManifests = append(pbManifests, &reconpb.CollectorManifest{
			Name:         m.Name,
			Version:      m.Version,
			Category:     m.Category,
			Description:  m.Description,
			Reads:        m.Reads,
			Requires:     reqs,
			ParamsSchema: body,
		})
	}

	facts := AutoFacts()
	labels := make(map[string]string, len(c.cfg.Identity.Labels)+len(facts))
	for k, v := range c.cfg.Identity.Labels {
		labels[k] = v
	}
	for k, v := range facts {
		// Auto-facts also become labels for selector matching (PROJECT.md §3.2).
		labels[k] = v
	}

	return send.Send(&reconpb.AgentMsg{Payload: &reconpb.AgentMsg_Hello{Hello: &reconpb.Hello{
		AgentId:    c.cfg.Identity.ID,
		Version:    version.Full(),
		Labels:     labels,
		Collectors: pbManifests,
		Facts:      facts,
	}}})
}

// capabilityProbeLoop re-runs collect.RefreshAvailability() on a fixed
// cadence (default 60s — coarse enough to not stat /usr/bin/* in a tight
// loop, fine enough that an `apt install docker` is reflected on the
// hub's /collectors page within a minute). On any diff, sends a fresh
// Hello: hub upserts hosts + DELETE+INSERTs collector_manifests, so the
// LLM investigator immediately starts seeing the newly-available probes
// (or stops seeing the removed ones) without an agent restart.
func (c *Client) capabilityProbeLoop(ctx context.Context, send *streamSender) {
	const interval = 60 * time.Second
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			diff := collect.RefreshAvailability()
			if !diff.Changed() {
				continue
			}
			c.log.Info("collector capability change — re-advertising manifests",
				"now_available", diff.NowAvailable,
				"now_unavailable", diff.NowUnavailable)
			if err := c.sendHello(send); err != nil {
				c.log.Warn("re-hello after capability change failed", "err", err)
			}
		}
	}
}

func (c *Client) heartbeatLoop(ctx context.Context, send *streamSender) {
	t := time.NewTicker(c.cfg.Runtime.HeartbeatInterval)
	defer t.Stop()
	start := time.Now()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			err := send.Send(&reconpb.AgentMsg{Payload: &reconpb.AgentMsg_Heartbeat{Heartbeat: &reconpb.Heartbeat{
				AgentId: c.cfg.Identity.ID,
				UptimeS: int64(time.Since(start).Seconds()),
				TsUnix:  time.Now().Unix(),
			}}})
			if err != nil {
				c.log.Warn("heartbeat send failed", "err", err)
				return
			}
		}
	}
}
