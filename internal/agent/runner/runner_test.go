package runner

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/vasyakrg/recon/internal/agent/collect"
	reconpb "github.com/vasyakrg/recon/internal/proto"
)

type fakeSender struct {
	mu   sync.Mutex
	msgs []*reconpb.AgentMsg
	err  error
}

func (f *fakeSender) Send(m *reconpb.AgentMsg) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	f.msgs = append(f.msgs, m)
	return nil
}

func (f *fakeSender) results() []*reconpb.CollectResult {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []*reconpb.CollectResult
	for _, m := range f.msgs {
		if r := m.GetResult(); r != nil {
			out = append(out, r)
		}
	}
	return out
}

func (f *fakeSender) artifacts() []*reconpb.ArtifactChunk {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []*reconpb.ArtifactChunk
	for _, m := range f.msgs {
		if a := m.GetArtifact(); a != nil {
			out = append(out, a)
		}
	}
	return out
}

func newRunner(t *testing.T) *Runner {
	t.Helper()
	return New(2, 2*time.Second, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

// fakeCollector that we register in tests.
type fakeCollector struct {
	manifest collect.Manifest
	run      func(ctx context.Context, p collect.Params) (collect.Result, error)
}

func (f *fakeCollector) Manifest() collect.Manifest { return f.manifest }
func (f *fakeCollector) Run(ctx context.Context, p collect.Params) (collect.Result, error) {
	return f.run(ctx, p)
}

// Helper that registers a collector under a unique name and unregisters at end.
func register(t *testing.T, name string, run func(context.Context, collect.Params) (collect.Result, error)) {
	t.Helper()
	collect.Register(&fakeCollector{
		manifest: collect.Manifest{Name: name, Version: "test", Category: "test", Description: name},
		run:      run,
	})
	t.Cleanup(func() { collect.UnregisterForTest(name) })
}

func waitForResult(t *testing.T, fs *fakeSender) *reconpb.CollectResult {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		rs := fs.results()
		if len(rs) > 0 {
			return rs[0]
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("no result received")
	return nil
}

func TestRunHappyPath(t *testing.T) {
	register(t, "ok-1", func(_ context.Context, _ collect.Params) (collect.Result, error) {
		return collect.Result{Data: map[string]any{"hello": "world"}}, nil
	})
	r := newRunner(t)
	fs := &fakeSender{}
	r.Handle(context.Background(), &reconpb.CollectRequest{RequestId: "r1", Collector: "ok-1"}, fs)

	got := waitForResult(t, fs)
	if got.Status != reconpb.Status_STATUS_OK {
		t.Fatalf("status=%v err=%q", got.Status, got.Error)
	}
	if string(got.DataJson) != `{"hello":"world"}` {
		t.Fatalf("data: %s", got.DataJson)
	}
}

func TestRunPanicRecovered(t *testing.T) {
	register(t, "panic-1", func(_ context.Context, _ collect.Params) (collect.Result, error) {
		panic("boom — simulating exec gateway disallowed call")
	})
	r := newRunner(t)
	fs := &fakeSender{}
	r.Handle(context.Background(), &reconpb.CollectRequest{RequestId: "r2", Collector: "panic-1"}, fs)

	got := waitForResult(t, fs)
	if got.Status != reconpb.Status_STATUS_ERROR {
		t.Fatalf("status=%v err=%q", got.Status, got.Error)
	}
	if got.Error == "" {
		t.Fatal("error should be populated on panic")
	}
}

func TestRunTimeout(t *testing.T) {
	register(t, "slow-1", func(ctx context.Context, _ collect.Params) (collect.Result, error) {
		<-ctx.Done()
		return collect.Result{}, ctx.Err()
	})
	r := newRunner(t)
	fs := &fakeSender{}
	r.Handle(context.Background(), &reconpb.CollectRequest{RequestId: "r3", Collector: "slow-1", TimeoutSeconds: 1}, fs)

	got := waitForResult(t, fs)
	if got.Status != reconpb.Status_STATUS_TIMEOUT {
		t.Fatalf("status=%v", got.Status)
	}
}

func TestRunUnknownCollector(t *testing.T) {
	r := newRunner(t)
	fs := &fakeSender{}
	r.Handle(context.Background(), &reconpb.CollectRequest{RequestId: "r4", Collector: "does-not-exist"}, fs)
	got := waitForResult(t, fs)
	if got.Status != reconpb.Status_STATUS_ERROR {
		t.Fatalf("status=%v", got.Status)
	}
}

func TestArtifactStreamed(t *testing.T) {
	body := make([]byte, artifactChunkSize*2+17)
	for i := range body {
		body[i] = byte(i % 251)
	}
	register(t, "art-1", func(_ context.Context, _ collect.Params) (collect.Result, error) {
		return collect.Result{
			Data:      "ok",
			Artifacts: []collect.Artifact{{Name: "big.bin", Mime: "application/octet-stream", Body: body}},
		}, nil
	})
	r := newRunner(t)
	fs := &fakeSender{}
	r.Handle(context.Background(), &reconpb.CollectRequest{RequestId: "r5", Collector: "art-1"}, fs)
	_ = waitForResult(t, fs)

	chunks := fs.artifacts()
	if len(chunks) != 3 {
		t.Fatalf("expected 3 chunks, got %d", len(chunks))
	}
	if !chunks[len(chunks)-1].Last {
		t.Fatal("last chunk must be marked Last")
	}
	var reassembled []byte
	for _, c := range chunks {
		reassembled = append(reassembled, c.Data...)
	}
	if len(reassembled) != len(body) {
		t.Fatalf("reassembled %d bytes, want %d", len(reassembled), len(body))
	}
}

func TestCancel(t *testing.T) {
	started := make(chan struct{})
	register(t, "cancel-1", func(ctx context.Context, _ collect.Params) (collect.Result, error) {
		close(started)
		<-ctx.Done()
		return collect.Result{}, ctx.Err()
	})
	r := newRunner(t)
	fs := &fakeSender{}
	r.Handle(context.Background(), &reconpb.CollectRequest{RequestId: "r6", Collector: "cancel-1", TimeoutSeconds: 30}, fs)
	<-started
	if !r.Cancel("r6") {
		t.Fatal("Cancel returned false")
	}
	got := waitForResult(t, fs)
	if got.Status != reconpb.Status_STATUS_CANCELED {
		t.Fatalf("status=%v", got.Status)
	}
}

func TestSendErrorDoesNotPanic(t *testing.T) {
	register(t, "ok-2", func(_ context.Context, _ collect.Params) (collect.Result, error) {
		return collect.Result{Data: 1}, nil
	})
	r := newRunner(t)
	fs := &fakeSender{err: errors.New("stream broken")}
	// Just ensure no panic; nothing observable since the only sink is broken.
	r.Handle(context.Background(), &reconpb.CollectRequest{RequestId: "r7", Collector: "ok-2"}, fs)
	time.Sleep(100 * time.Millisecond)
}
