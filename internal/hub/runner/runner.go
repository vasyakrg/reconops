// Package runner orchestrates Run / Task lifecycle on the hub side. It
// fans out CollectRequest to the selected agents, persists tasks/results,
// and accumulates ArtifactChunk streams to disk.
package runner

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"

	"github.com/vasyakrg/recon/internal/hub/store"
	reconpb "github.com/vasyakrg/recon/internal/proto"
)

// Dispatcher is the subset of api.Server that the runner needs.
type Dispatcher interface {
	SendCollect(agentID string, req *reconpb.CollectRequest) bool
	IsOnline(agentID string) bool
	OnlineAgents() []string
}

type Runner struct {
	store       *store.Store
	disp        Dispatcher
	artifactDir string
	log         *slog.Logger

	mu       sync.RWMutex
	pending  map[string]*pendingTask // request_id -> task
	openArts map[string]*openArtifact
}

type pendingTask struct {
	taskID    string
	runID     string
	hostID    string
	collector string
	done      chan struct{} // closed when result has been processed
}

type openArtifact struct {
	taskID string
	name   string
	mime   string
	file   *os.File
	dir    string
}

// New creates a Runner. The artifactDir is the per-task root; results are
// stored under {artifactDir}/{task_id}/{name}.
func New(st *store.Store, disp Dispatcher, artifactDir string, log *slog.Logger) *Runner {
	return &Runner{
		store:       st,
		disp:        disp,
		artifactDir: artifactDir,
		log:         log,
		pending:     map[string]*pendingTask{},
		openArts:    map[string]*openArtifact{},
	}
}

type RunRequest struct {
	Name      string
	HostIDs   []string
	Collector string
	Params    map[string]string
	Timeout   int32
	CreatedBy string
}

// CreateRun persists a Run + per-host Tasks, dispatches CollectRequest to
// each online agent, and returns the new run id. Tasks for offline hosts
// are marked status='undeliverable' immediately. The function does NOT
// wait for results — UI polls /runs/{id} or, in later weeks, subscribes via SSE.
func (r *Runner) CreateRun(ctx context.Context, req RunRequest) (string, error) {
	if len(req.HostIDs) == 0 {
		return "", fmt.Errorf("no hosts selected")
	}
	if req.Collector == "" {
		return "", fmt.Errorf("collector required")
	}
	runID := newID("run")
	if err := r.store.InsertRun(ctx, store.Run{
		ID:        runID,
		Name:      req.Name,
		Selector:  map[string]string{"hosts": fmt.Sprintf("%d", len(req.HostIDs))},
		CreatedBy: req.CreatedBy,
		Status:    "running",
	}); err != nil {
		return "", err
	}

	for _, hostID := range req.HostIDs {
		taskID := newID("task")
		if err := r.store.InsertTask(ctx, store.Task{
			ID:        taskID,
			RunID:     runID,
			HostID:    hostID,
			Collector: req.Collector,
			Params:    req.Params,
			Status:    "pending",
		}); err != nil {
			r.log.Error("insert task", "err", err)
			continue
		}

		pbReq := &reconpb.CollectRequest{
			RequestId:      taskID,
			Collector:      req.Collector,
			Params:         req.Params,
			TimeoutSeconds: req.Timeout,
		}

		if !r.disp.IsOnline(hostID) {
			_ = r.store.FinishTask(ctx, taskID, "undeliverable", 0, "agent offline")
			continue
		}

		r.register(taskID, runID, hostID, req.Collector)

		if !r.disp.SendCollect(hostID, pbReq) {
			r.unregister(taskID)
			_ = r.store.FinishTask(ctx, taskID, "undeliverable", 0, "send queue full or stream closed")
			continue
		}
		_ = r.store.StartTask(ctx, taskID)
	}

	// Mark run done eventually — for week 2 the simplest is to leave it
	// 'running' and let a poller close it once all tasks are terminal.
	//nolint:gosec // G118: poller outlives the request ctx by design
	go r.watchRunCompletion(runID)
	return runID, nil
}

func (r *Runner) register(taskID, runID, hostID, collector string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.pending[taskID] = &pendingTask{
		taskID: taskID, runID: runID, hostID: hostID, collector: collector,
		done: make(chan struct{}),
	}
}

func (r *Runner) unregister(taskID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if pt, ok := r.pending[taskID]; ok {
		close(pt.done)
		delete(r.pending, taskID)
	}
}

// OnResult implements api.ResultSink.
func (r *Runner) OnResult(agentID string, res *reconpb.CollectResult) {
	r.mu.RLock()
	pt, ok := r.pending[res.RequestId]
	r.mu.RUnlock()
	if !ok {
		r.log.Warn("result for unknown task", "request_id", res.RequestId, "agent", agentID)
		return
	}
	if pt.hostID != agentID {
		r.log.Warn("result from wrong agent", "request_id", res.RequestId, "expected", pt.hostID, "got", agentID)
		return
	}
	status := mapStatus(res.Status)
	ctx := context.Background()
	if err := r.store.FinishTask(ctx, res.RequestId, status, res.DurationMs, res.Error); err != nil {
		r.log.Error("finish task", "task_id", res.RequestId, "err", err)
	}
	hintsJSON, _ := json.Marshal(res.Hints)
	artDir := filepath.Join(r.artifactDir, res.RequestId)
	if err := r.store.UpsertResult(ctx, store.Result{
		TaskID:      res.RequestId,
		DataJSON:    res.DataJson,
		HintsJSON:   hintsJSON,
		Stderr:      res.Stderr,
		ExitCode:    int(res.ExitCode),
		ArtifactDir: artDir,
	}); err != nil {
		r.log.Error("upsert result", "task_id", res.RequestId, "err", err)
	}
	r.unregister(res.RequestId)
	r.log.Info("task finished", "task_id", res.RequestId, "agent", agentID, "status", status, "duration_ms", res.DurationMs)
}

// OnArtifact implements api.ResultSink.
func (r *Runner) OnArtifact(agentID string, a *reconpb.ArtifactChunk) {
	r.mu.Lock()
	defer r.mu.Unlock()
	pt, ok := r.pending[a.RequestId]
	if !ok {
		r.log.Warn("artifact for unknown task", "request_id", a.RequestId)
		return
	}
	if pt.hostID != agentID {
		r.log.Warn("artifact from wrong agent", "request_id", a.RequestId, "expected", pt.hostID, "got", agentID)
		return
	}
	open, ok := r.openArts[a.ArtifactId]
	if !ok {
		dir := filepath.Join(r.artifactDir, a.RequestId)
		if err := os.MkdirAll(dir, 0o750); err != nil {
			r.log.Error("mkdir artifact dir", "err", err)
			return
		}
		f, err := os.Create(filepath.Join(dir, sanitize(a.Name)))
		if err != nil {
			r.log.Error("create artifact", "err", err)
			return
		}
		open = &openArtifact{taskID: a.RequestId, name: a.Name, mime: a.Mime, file: f, dir: dir}
		r.openArts[a.ArtifactId] = open
	}
	if len(a.Data) > 0 {
		if _, err := open.file.Write(a.Data); err != nil {
			r.log.Error("write artifact chunk", "err", err)
		}
	}
	if a.Last {
		_ = open.file.Close()
		delete(r.openArts, a.ArtifactId)
	}
}

// watchRunCompletion polls task statuses and finalizes the run when all
// tasks reach a terminal state. Cheap because we look only at this run.
func (r *Runner) watchRunCompletion(runID string) {
	for {
		tasks, err := r.store.ListTasks(context.Background(), runID)
		if err != nil {
			r.log.Error("list tasks", "err", err)
			return
		}
		allDone := true
		anyError := false
		for _, t := range tasks {
			if !isTerminal(t.Status) {
				allDone = false
				break
			}
			if t.Status != "ok" {
				anyError = true
			}
		}
		if allDone {
			final := "done"
			if anyError {
				final = "done"
			}
			_ = r.store.FinishRun(context.Background(), runID, final)
			return
		}
		// Sleep a bit; for week 2 polling is fine.
		select {
		case <-context.Background().Done():
			return
		case <-after(500):
		}
	}
}

func isTerminal(status string) bool {
	switch status {
	case "ok", "error", "timeout", "canceled", "undeliverable":
		return true
	}
	return false
}

func mapStatus(s reconpb.Status) string {
	switch s {
	case reconpb.Status_STATUS_OK:
		return "ok"
	case reconpb.Status_STATUS_ERROR:
		return "error"
	case reconpb.Status_STATUS_TIMEOUT:
		return "timeout"
	case reconpb.Status_STATUS_CANCELED:
		return "canceled"
	}
	return "error"
}

func newID(prefix string) string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return prefix + "_" + hex.EncodeToString(b[:])
}

func sanitize(name string) string {
	out := make([]byte, 0, len(name))
	for i := 0; i < len(name); i++ {
		c := name[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9',
			c == '.', c == '-', c == '_':
			out = append(out, c)
		default:
			out = append(out, '_')
		}
	}
	if len(out) == 0 {
		return "artifact"
	}
	return string(out)
}
