// Package runner executes incoming CollectRequest payloads on the agent
// side. Every collector invocation is wrapped in recover() — the read-only
// exec gateway intentionally panics on disallowed (bin, args) pairs, and we
// must not let that crash the agent (PROJECT.md §14: "крэш бинаря недопустим").
package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"
	"sync"
	"time"

	"github.com/vasyakrg/recon/internal/agent/collect"
	reconpb "github.com/vasyakrg/recon/internal/proto"
)

// Sender is the side-effect interface a runner needs to push results back to
// the hub. The agent.conn package implements this via the bidi gRPC stream.
type Sender interface {
	Send(*reconpb.AgentMsg) error
}

type Runner struct {
	sem            chan struct{}
	defaultTimeout time.Duration
	log            *slog.Logger

	mu      sync.Mutex
	running map[string]context.CancelFunc // request_id → cancel
}

func New(maxConcurrent int, defaultTimeout time.Duration, log *slog.Logger) *Runner {
	if maxConcurrent < 1 {
		maxConcurrent = 1
	}
	return &Runner{
		sem:            make(chan struct{}, maxConcurrent),
		defaultTimeout: defaultTimeout,
		log:            log,
		running:        map[string]context.CancelFunc{},
	}
}

// Handle runs the request asynchronously. It returns immediately; the result
// is sent through send when complete. Concurrency is bounded by the runner's
// semaphore — overflowing requests block until capacity frees up.
func (r *Runner) Handle(parent context.Context, req *reconpb.CollectRequest, send Sender) {
	go r.run(parent, req, send)
}

// Cancel attempts to abort an in-flight request. Returns true if a matching
// request was found and cancellation was issued.
func (r *Runner) Cancel(requestID string) bool {
	r.mu.Lock()
	cancel, ok := r.running[requestID]
	r.mu.Unlock()
	if !ok {
		return false
	}
	cancel()
	return true
}

func (r *Runner) run(parent context.Context, req *reconpb.CollectRequest, send Sender) {
	// Pre-bound cap on inflight collectors. Backpressure: if overloaded, this
	// blocks the caller — the gRPC reader goroutine keeps draining since
	// Handle launched a goroutine.
	select {
	case r.sem <- struct{}{}:
	case <-parent.Done():
		return
	}
	defer func() { <-r.sem }()

	timeout := time.Duration(req.TimeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = r.defaultTimeout
	}
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()

	r.mu.Lock()
	r.running[req.RequestId] = cancel
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		delete(r.running, req.RequestId)
		r.mu.Unlock()
	}()

	start := time.Now()
	res, err := r.invoke(ctx, req)
	durMs := time.Since(start).Milliseconds()

	pb := &reconpb.CollectResult{
		RequestId:  req.RequestId,
		DurationMs: durMs,
	}
	switch {
	case err == nil:
		pb.Status = reconpb.Status_STATUS_OK
		pb.DataJson, _ = json.Marshal(res.Data)
		pb.Hints = hintsToProto(res.Hints)
		pb.Stderr = res.Stderr
		pb.ExitCode = int32(res.ExitCode) //nolint:gosec // exit codes are 0..255
	case errors.Is(err, context.DeadlineExceeded):
		pb.Status = reconpb.Status_STATUS_TIMEOUT
		pb.Error = fmt.Sprintf("timeout after %s", timeout)
	case errors.Is(err, context.Canceled):
		pb.Status = reconpb.Status_STATUS_CANCELED
		pb.Error = "canceled"
	default:
		pb.Status = reconpb.Status_STATUS_ERROR
		pb.Error = err.Error()
	}

	// Stream artifacts (best-effort — failure to send chunks does not flip
	// the result status, but is logged).
	for _, art := range res.Artifacts {
		if err := r.streamArtifact(req.RequestId, art, send); err != nil {
			r.log.Warn("artifact stream failed", "request_id", req.RequestId, "name", art.Name, "err", err)
		} else {
			pb.ArtifactRefs = append(pb.ArtifactRefs, art.Name)
		}
	}

	if err := send.Send(&reconpb.AgentMsg{Payload: &reconpb.AgentMsg_Result{Result: pb}}); err != nil {
		r.log.Warn("send result failed", "request_id", req.RequestId, "err", err)
	}
}

// invoke performs the actual collector call inside a defer/recover so a
// panicking collector (or the exec gateway's intentional panic on disallowed
// args) does not propagate upward.
func (r *Runner) invoke(ctx context.Context, req *reconpb.CollectRequest) (res collect.Result, err error) {
	defer func() {
		if rec := recover(); rec != nil {
			r.log.Error("collector panic recovered",
				"request_id", req.RequestId,
				"collector", req.Collector,
				"panic", rec,
				"stack", string(debug.Stack()))
			err = fmt.Errorf("collector panic: %v", rec)
		}
	}()

	c, ok := collect.Get(req.Collector)
	if !ok {
		return collect.Result{}, fmt.Errorf("collector %q not registered", req.Collector)
	}
	return c.Run(ctx, collect.Params(req.Params))
}

func hintsToProto(hs []collect.Hint) []*reconpb.Hint {
	out := make([]*reconpb.Hint, 0, len(hs))
	for _, h := range hs {
		body, _ := json.Marshal(h.Evidence)
		out = append(out, &reconpb.Hint{
			Severity:     h.Severity,
			Code:         h.Code,
			Message:      h.Message,
			EvidenceJson: body,
		})
	}
	return out
}

const artifactChunkSize = 64 * 1024

func (r *Runner) streamArtifact(reqID string, art collect.Artifact, send Sender) error {
	id := fmt.Sprintf("%s/%s", reqID, art.Name)
	body := art.Body
	if len(body) == 0 {
		// Send a single empty terminating chunk so the hub records the artifact.
		return send.Send(&reconpb.AgentMsg{Payload: &reconpb.AgentMsg_Artifact{Artifact: &reconpb.ArtifactChunk{
			RequestId: reqID, ArtifactId: id, Name: art.Name, Mime: art.Mime, Last: true,
		}}})
	}
	var offset int64
	for len(body) > 0 {
		n := artifactChunkSize
		if n > len(body) {
			n = len(body)
		}
		chunk := &reconpb.ArtifactChunk{
			RequestId:  reqID,
			ArtifactId: id,
			Name:       art.Name,
			Mime:       art.Mime,
			Offset:     offset,
			Data:       body[:n],
			Last:       n == len(body),
		}
		if err := send.Send(&reconpb.AgentMsg{Payload: &reconpb.AgentMsg_Artifact{Artifact: chunk}}); err != nil {
			return err
		}
		offset += int64(n)
		body = body[n:]
	}
	return nil
}
