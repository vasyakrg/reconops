// Package api implements the gRPC service exposed to agents (Hub.Enroll +
// Hub.Connect). One listener serves both endpoints. Clients without a
// verified client cert may only call Enroll; Connect requires mTLS. This is
// enforced in the unary/stream interceptors below.
package api

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"time"

	"google.golang.org/grpc"
	codes "google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"

	"github.com/vasyakrg/recon/internal/hub/auth"
	"github.com/vasyakrg/recon/internal/hub/store"
	reconpb "github.com/vasyakrg/recon/internal/proto"
)

type Server struct {
	reconpb.UnimplementedHubServer

	store    *store.Store
	pki      *auth.Material
	clientCA *x509.CertPool
	log      *slog.Logger

	mu      sync.RWMutex
	streams map[string]*streamHandle // agentID -> stream

	clientCertTTL time.Duration
}

type streamHandle struct {
	send   chan *reconpb.HubMsg
	cancel context.CancelFunc
}

func NewServer(st *store.Store, pki *auth.Material, log *slog.Logger) *Server {
	pool := x509.NewCertPool()
	pool.AddCert(pki.CACert)
	return &Server{
		store:         st,
		pki:           pki,
		clientCA:      pool,
		log:           log,
		streams:       map[string]*streamHandle{},
		clientCertTTL: 90 * 24 * time.Hour,
	}
}

func (s *Server) Listen(addr string) (net.Listener, *grpc.Server, error) {
	cert, err := tls.X509KeyPair(s.pki.ServerCert, s.pki.ServerKey)
	if err != nil {
		return nil, nil, fmt.Errorf("server keypair: %w", err)
	}
	tlsCfg := &tls.Config{
		Certificates: []tls.Certificate{cert},
		ClientCAs:    s.clientCA,
		// Some calls (Enroll) precede the agent having a cert. Verify if
		// presented; the interceptor enforces presence on Connect.
		ClientAuth: tls.VerifyClientCertIfGiven,
		MinVersion: tls.VersionTLS12,
	}
	creds := credentials.NewTLS(tlsCfg)

	srv := grpc.NewServer(
		grpc.Creds(creds),
		grpc.UnaryInterceptor(s.unaryAuth),
		grpc.StreamInterceptor(s.streamAuth),
	)
	reconpb.RegisterHubServer(srv, s)

	lis, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, nil, err
	}
	return lis, srv, nil
}

func (s *Server) unaryAuth(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
	// Enroll is intentionally accessible without a client cert.
	return handler(ctx, req)
}

func (s *Server) streamAuth(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
	if _, err := requireVerifiedPeer(ss.Context()); err != nil {
		return status.Error(codes.Unauthenticated, err.Error())
	}
	return handler(srv, ss)
}

func requireVerifiedPeer(ctx context.Context) (*x509.Certificate, error) {
	p, ok := peer.FromContext(ctx)
	if !ok {
		return nil, errors.New("no peer info")
	}
	tlsInfo, ok := p.AuthInfo.(credentials.TLSInfo)
	if !ok {
		return nil, errors.New("non-TLS peer")
	}
	chains := tlsInfo.State.VerifiedChains
	if len(chains) == 0 || len(chains[0]) == 0 {
		return nil, errors.New("client cert not presented or not verified")
	}
	return chains[0][0], nil
}

// ---------------------------------------------------------------------------
// Enroll
// ---------------------------------------------------------------------------

func (s *Server) Enroll(ctx context.Context, req *reconpb.EnrollRequest) (*reconpb.EnrollResponse, error) {
	if req.AgentId == "" || req.BootstrapToken == "" || len(req.CsrPem) == 0 {
		return nil, status.Error(codes.InvalidArgument, "agent_id, bootstrap_token, csr_pem are required")
	}
	// Token must be bound to this exact agent_id (C2). Atomic.
	if err := s.store.ConsumeBootstrapToken(ctx, req.BootstrapToken, req.AgentId); err != nil {
		s.log.Warn("enroll rejected: token", "agent_id", req.AgentId, "err", err)
		return nil, status.Error(codes.PermissionDenied, "invalid or expired token")
	}
	// Refuse re-enroll under the same agent_id when an active (non-revoked)
	// identity already exists (C3). VerifyIdentity returns ErrFingerprintMismatch
	// for a non-revoked row when we pass an empty fingerprint, since "" never
	// matches the stored value. ErrIdentityRevoked means the operator wants
	// re-enrollment — RegisterIdentity will replace the row atomically.
	if err := s.store.VerifyIdentity(ctx, req.AgentId, ""); err == nil || errors.Is(err, store.ErrFingerprintMismatch) {
		s.log.Warn("enroll rejected: identity already active", "agent_id", req.AgentId)
		return nil, status.Error(codes.AlreadyExists, "agent_id already enrolled — revoke first")
	}
	certPEM, err := s.pki.SignCSR(req.CsrPem, req.AgentId, s.clientCertTTL)
	if err != nil {
		s.log.Error("sign csr", "agent_id", req.AgentId, "err", err)
		return nil, status.Error(codes.InvalidArgument, "csr rejected")
	}
	fingerprint, err := auth.FingerprintFromPEM(certPEM)
	if err != nil {
		s.log.Error("fingerprint", "agent_id", req.AgentId, "err", err)
		return nil, status.Error(codes.Internal, "internal cert error")
	}
	if err := s.store.RegisterIdentity(ctx, req.AgentId, fingerprint, "bootstrap"); err != nil {
		// Race: another Enroll for the same agent_id sneaked in. Token is
		// already consumed, identity row exists — we reject this caller.
		s.log.Warn("register identity", "agent_id", req.AgentId, "err", err)
		return nil, status.Error(codes.AlreadyExists, "agent_id already enrolled")
	}
	_ = s.store.AuditLog(ctx, "agent:"+req.AgentId, "enroll", map[string]any{
		"agent_id":    req.AgentId,
		"fingerprint": fingerprint,
	})
	s.log.Info("agent enrolled", "agent_id", req.AgentId, "fingerprint", fingerprint)
	return &reconpb.EnrollResponse{ClientCertPem: certPEM, HubCaPem: s.pki.CACertPEM}, nil
}

// ---------------------------------------------------------------------------
// Connect (bidi stream)
// ---------------------------------------------------------------------------

func (s *Server) Connect(ss reconpb.Hub_ConnectServer) error {
	cert, err := requireVerifiedPeer(ss.Context())
	if err != nil {
		return status.Error(codes.Unauthenticated, err.Error())
	}
	fingerprint := auth.FingerprintFromCert(cert)
	agentID := cert.Subject.CommonName
	if agentID == "" {
		return status.Error(codes.Unauthenticated, "client cert missing CN")
	}

	// (C1) The cert was signed by our CA at some point, but that alone is
	// not enough — we require an active enrolled identity matching this
	// (agent_id, fingerprint). Revoked or stolen-and-re-issued certs are
	// rejected here.
	if err := s.store.VerifyIdentity(ss.Context(), agentID, fingerprint); err != nil {
		_ = s.store.AuditLog(ss.Context(), "agent:"+agentID, "connect.rejected", map[string]any{
			"agent_id":    agentID,
			"fingerprint": fingerprint,
			"reason":      err.Error(),
		})
		s.log.Warn("connect rejected", "agent_id", agentID, "fingerprint", fingerprint, "err", err)
		return status.Error(codes.PermissionDenied, "identity verification failed")
	}

	// First message must be Hello.
	first, err := ss.Recv()
	if err != nil {
		return err
	}
	hello := first.GetHello()
	if hello == nil {
		return status.Error(codes.FailedPrecondition, "first message must be Hello")
	}
	if hello.AgentId != agentID {
		return status.Errorf(codes.PermissionDenied, "Hello.agent_id %q does not match cert CN %q", hello.AgentId, agentID)
	}
	if err := s.applyHello(ss.Context(), hello, fingerprint); err != nil {
		return status.Errorf(codes.Internal, "apply hello: %v", err)
	}

	streamCtx, cancel := context.WithCancel(ss.Context())
	defer cancel()
	handle := &streamHandle{send: make(chan *reconpb.HubMsg, 8), cancel: cancel}
	s.registerStream(agentID, handle)
	defer s.unregisterStream(agentID)

	s.log.Info("agent connected", "agent_id", agentID, "fingerprint", fingerprint)

	// Pump outbound messages.
	go func() {
		for {
			select {
			case <-streamCtx.Done():
				return
			case msg := <-handle.send:
				if err := ss.Send(msg); err != nil {
					s.log.Warn("send to agent failed", "agent_id", agentID, "err", err)
					cancel()
					return
				}
			}
		}
	}()

	// Read inbound until error/EOF.
	for {
		msg, err := ss.Recv()
		if err != nil {
			s.log.Info("agent disconnected", "agent_id", agentID, "err", err)
			_ = s.store.TouchHost(context.Background(), agentID, "offline")
			return nil
		}
		switch p := msg.Payload.(type) {
		case *reconpb.AgentMsg_Heartbeat:
			_ = s.store.TouchHost(streamCtx, agentID, "online")
			_ = p
		case *reconpb.AgentMsg_Result:
			// Week 1: nothing to do with results yet — the runner will pick
			// these up in Week 2. Log so we can see them in the demo.
			s.log.Info("collect result", "agent_id", agentID, "request_id", p.Result.RequestId, "status", p.Result.Status.String())
		case *reconpb.AgentMsg_Artifact:
			s.log.Info("artifact chunk", "agent_id", agentID, "name", p.Artifact.Name, "bytes", len(p.Artifact.Data))
		case *reconpb.AgentMsg_Hello:
			// Re-Hello (e.g. after reconnect) — refresh inventory. Identity
			// was verified at session start; re-Hello cannot change CN/fp.
			if p.Hello.AgentId != agentID {
				s.log.Warn("re-hello: agent_id mismatch", "expected", agentID, "got", p.Hello.AgentId)
				return status.Error(codes.PermissionDenied, "re-hello agent_id mismatch")
			}
			if err := s.applyHello(streamCtx, p.Hello, fingerprint); err != nil {
				s.log.Warn("re-hello apply", "err", err)
			}
		}
	}
}

// applyHello records / refreshes the host row + collector manifests. The
// (agent_id, fingerprint) pair is already verified against enrolled_identities
// before this is called — applyHello does NOT perform identity checks.
func (s *Server) applyHello(ctx context.Context, hello *reconpb.Hello, fingerprint string) error {
	host := store.Host{
		ID:              hello.AgentId,
		AgentVersion:    hello.Version,
		Labels:          hello.Labels,
		Facts:           hello.Facts,
		CertFingerprint: fingerprint,
		Status:          "online",
	}
	if err := s.store.UpsertHost(ctx, host); err != nil {
		return err
	}
	mans := make([]store.CollectorManifest, 0, len(hello.Collectors))
	for _, m := range hello.Collectors {
		body, _ := json.Marshal(m)
		mans = append(mans, store.CollectorManifest{
			HostID:       hello.AgentId,
			Name:         m.Name,
			Version:      m.Version,
			ManifestJSON: body,
		})
	}
	return s.store.ReplaceCollectorManifests(ctx, hello.AgentId, mans)
}

func (s *Server) registerStream(agentID string, h *streamHandle) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if old, ok := s.streams[agentID]; ok {
		old.cancel()
	}
	s.streams[agentID] = h
}

func (s *Server) unregisterStream(agentID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.streams, agentID)
}

// SendCollect pushes a CollectRequest to the named agent. Returns false if
// the agent is not currently connected. Used by the runner in week 2 — week
// 1 keeps it for completeness.
func (s *Server) SendCollect(agentID string, req *reconpb.CollectRequest) bool {
	s.mu.RLock()
	h, ok := s.streams[agentID]
	s.mu.RUnlock()
	if !ok {
		return false
	}
	select {
	case h.send <- &reconpb.HubMsg{Payload: &reconpb.HubMsg_Collect{Collect: req}}:
		return true
	default:
		return false
	}
}
