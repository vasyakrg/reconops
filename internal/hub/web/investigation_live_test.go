package web

import (
	"bufio"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vasyakrg/recon/internal/hub/investigator"
	"github.com/vasyakrg/recon/internal/hub/store"
)

// These tests cover the investigation live-update / approve loop that had four
// "fix" commits over two days with no regression coverage. They pin the
// server-side contract the client live engine depends on:
//   - approve → advance → the fragment shows the NEXT pending step (not stuck
//     on "Waiting for the model.")
//   - the SSE fingerprint changes when state moves waiting → pending
//   - action handlers content-negotiate: fragments for fetch, 303 otherwise
//   - the SSE channel pushes an initial snapshot on connect and re-emits on a
//     Bus event

// TestInvestigationFragmentAdvancesToNextPending verifies that once the loop
// advances past an approved step, the live fragment surfaces the next pending
// approval rather than leaving the operator on "Waiting for the model." — the
// exact stuck-page symptom this feature fixes.
func TestInvestigationFragmentAdvancesToNextPending(t *testing.T) {
	srv, st := newTestServer(t)
	ctx := context.Background()
	if err := st.InsertInvestigation(ctx, store.Investigation{
		ID: "inv-advance", Goal: "diagnose nginx 502", Status: "active",
		CreatedBy: "operator", Model: "m", BaseURL: "https://llm.example/v1",
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.InsertToolCall(ctx, store.ToolCallRow{
		ID: "tc-step1", InvestigationID: "inv-advance", Seq: 1,
		Tool: "collect", InputJSON: `{"host_id":"app01"}`, Rationale: "check nginx logs", Status: "pending",
	}); err != nil {
		t.Fatal(err)
	}
	sid, _, err := srv.sessions.issue(ctx, "admin", time.Hour)
	if err != nil {
		t.Fatal(err)
	}

	snapBefore, err := srv.snapshotForSSE(ctx, "inv-advance")
	if err != nil {
		t.Fatal(err)
	}
	if body := fetchFragment(t, srv, sid, "inv-advance"); !strings.Contains(body, `name="tool_call_id" value="tc-step1"`) {
		t.Fatalf("fragment should show the first pending step, got:\n%s", body)
	}

	// Simulate the loop advancing: step 1 executes, step 2 becomes pending.
	if err := st.UpdateToolCall(ctx, "tc-step1", "executed", "operator", "", `{"ok":true}`); err != nil {
		t.Fatal(err)
	}
	if err := st.InsertToolCall(ctx, store.ToolCallRow{
		ID: "tc-step2", InvestigationID: "inv-advance", Seq: 2,
		Tool: "exec", InputJSON: `{"host_id":"app01","cmd":"nginx -t"}`, Rationale: "validate config", Status: "pending",
	}); err != nil {
		t.Fatal(err)
	}

	snapAfter, err := srv.snapshotForSSE(ctx, "inv-advance")
	if err != nil {
		t.Fatal(err)
	}
	if snapBefore == snapAfter {
		t.Fatalf("snapshot must change after the step advances: %s", snapAfter)
	}
	body := fetchFragment(t, srv, sid, "inv-advance")
	if !strings.Contains(body, `name="tool_call_id" value="tc-step2"`) {
		t.Fatalf("fragment should advance to the next pending step, got:\n%s", body)
	}
	if strings.Contains(body, `name="tool_call_id" value="tc-step1"`) {
		t.Fatalf("fragment should no longer show the resolved step:\n%s", body)
	}
	if strings.Contains(body, "Waiting for the model.") {
		t.Fatalf("a pending step must not render as Waiting for the model:\n%s", body)
	}
}

// A completed (done) investigation with the investigator configured must render
// an in-place continue form (the operator-chosen reopen UX) rather than only a
// terminal badge — the regression the done-continue work fixes.
func TestInvestigationDoneRendersContinueForm(t *testing.T) {
	srv, st := newTestServer(t)
	srv.availability.Enabled = true // investigator configured → done is reopenable
	ctx := context.Background()
	if err := st.InsertInvestigation(ctx, store.Investigation{
		ID: "inv-done-web", Goal: "diagnose", Status: "active",
		CreatedBy: "operator", Model: "m", BaseURL: "https://llm.example/v1",
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.FinishInvestigation(ctx, "inv-done-web", "done",
		store.TerminalDonePayload(`{"root_cause":"tpm storm"}`, time.Time{}).JSON()); err != nil {
		t.Fatal(err)
	}
	sid, _, err := srv.sessions.issue(ctx, "admin", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	body := fetchFragment(t, srv, sid, "inv-done-web")
	if !strings.Contains(body, `action="/investigations/continue"`) {
		t.Fatalf("a done investigation must render a continue form, got:\n%s", body)
	}
	if !strings.Contains(body, "Completed — continue investigation") {
		t.Fatalf("done continue form header missing, got:\n%s", body)
	}
}

// The per-region content gate (flicker fix): token/budget churn lives only in
// the status fragment, so the timeline and side region hashes must stay
// IDENTICAL across a pure token bump (the exact churn the old global
// data-snapshot fingerprint reflowed on), while the status hash changes. A real
// timeline change (new tool call) must move the timeline hash.
func TestPerRegionHashIsolatesTokenChurnFromTimeline(t *testing.T) {
	srv, st := newTestServer(t)
	ctx := context.Background()
	if err := st.InsertInvestigation(ctx, store.Investigation{
		ID: "inv-hash", Goal: "diagnose", Status: "active",
		CreatedBy: "operator", Model: "m", BaseURL: "https://llm.example/v1",
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.InsertToolCall(ctx, store.ToolCallRow{
		ID: "tc1", InvestigationID: "inv-hash", Seq: 1, Tool: "collect",
		InputJSON: `{"host_id":"h1"}`, Rationale: "check", Status: "pending",
	}); err != nil {
		t.Fatal(err)
	}

	d1, err := srv.investigationDetailData(ctx, "inv-hash")
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"StatusHash", "TimelineHash", "SideHash"} {
		if d1[k] == "" {
			t.Fatalf("%s must be populated", k)
		}
	}
	// Pure token churn — the exact thing the old global fingerprint reflowed on.
	if err := st.AccumulateTokens(ctx, "inv-hash", 120_000, 80_000); err != nil {
		t.Fatal(err)
	}
	d2, err := srv.investigationDetailData(ctx, "inv-hash")
	if err != nil {
		t.Fatal(err)
	}
	if d1["TimelineHash"] != d2["TimelineHash"] {
		t.Fatalf("timeline hash must be STABLE across token churn (%v -> %v)", d1["TimelineHash"], d2["TimelineHash"])
	}
	if d1["SideHash"] != d2["SideHash"] {
		t.Fatalf("side hash must be STABLE across token churn (%v -> %v)", d1["SideHash"], d2["SideHash"])
	}
	if d1["StatusHash"] == d2["StatusHash"] {
		t.Fatalf("status hash must CHANGE on token churn (budget bar updated), got stable %v", d1["StatusHash"])
	}

	// A real timeline change (a new tool call) must move the timeline hash.
	if err := st.InsertToolCall(ctx, store.ToolCallRow{
		ID: "tc2", InvestigationID: "inv-hash", Seq: 2, Tool: "exec",
		InputJSON: `{"host_id":"h1","cmd":"nginx -t"}`, Rationale: "validate", Status: "pending",
	}); err != nil {
		t.Fatal(err)
	}
	d3, err := srv.investigationDetailData(ctx, "inv-hash")
	if err != nil {
		t.Fatal(err)
	}
	if d2["TimelineHash"] == d3["TimelineHash"] {
		t.Fatalf("timeline hash must change when a tool call is added, got stable %v", d2["TimelineHash"])
	}
}

// TestInvestigationSnapshotChangesWaitingToPending verifies the SSE fingerprint
// (and the fragment) move from the "Waiting for the model." state to a pending
// approval when the model proposes a tool_call — the signal the live engine
// uses to wake the page.
func TestInvestigationSnapshotChangesWaitingToPending(t *testing.T) {
	srv, st := newTestServer(t)
	ctx := context.Background()
	if err := st.InsertInvestigation(ctx, store.Investigation{
		ID: "inv-wait", Goal: "diagnose nginx 502", Status: "active",
		CreatedBy: "operator", Model: "m", BaseURL: "https://llm.example/v1",
	}); err != nil {
		t.Fatal(err)
	}
	sid, _, err := srv.sessions.issue(ctx, "admin", time.Hour)
	if err != nil {
		t.Fatal(err)
	}

	waitingSnap, err := srv.snapshotForSSE(ctx, "inv-wait")
	if err != nil {
		t.Fatal(err)
	}
	if body := fetchFragment(t, srv, sid, "inv-wait"); !strings.Contains(body, "Waiting for the model.") {
		t.Fatalf("active investigation with no pending step should render Waiting for the model, got:\n%s", body)
	}

	if err := st.InsertToolCall(ctx, store.ToolCallRow{
		ID: "tc-wp", InvestigationID: "inv-wait", Seq: 1,
		Tool: "collect", InputJSON: `{"host_id":"app01"}`, Status: "pending",
	}); err != nil {
		t.Fatal(err)
	}

	pendingSnap, err := srv.snapshotForSSE(ctx, "inv-wait")
	if err != nil {
		t.Fatal(err)
	}
	if waitingSnap == pendingSnap {
		t.Fatalf("snapshot must change waiting → pending: %s", pendingSnap)
	}
	if body := fetchFragment(t, srv, sid, "inv-wait"); strings.Contains(body, "Waiting for the model.") {
		t.Fatalf("a pending step must replace the Waiting for the model placeholder:\n%s", body)
	}
}

// TestInvestigationSnapshotTracksBudgetPauseFields verifies the live
// fingerprint follows the budget fields the detail page renders. Without
// these counters a page can stay on the stale "waiting" view until a full F5,
// even though the next server render already shows the budget-extension card.
func TestInvestigationSnapshotTracksBudgetPauseFields(t *testing.T) {
	srv, st := newTestServer(t)
	ctx := context.Background()
	if err := st.InsertInvestigation(ctx, store.Investigation{
		ID: "inv-budget-live", Goal: "diagnose token cap", Status: "active",
		CreatedBy: "operator", Model: "m", BaseURL: "https://llm.example/v1",
	}); err != nil {
		t.Fatal(err)
	}
	sid, _, err := srv.sessions.issue(ctx, "admin", time.Hour)
	if err != nil {
		t.Fatal(err)
	}

	snapBefore, err := srv.snapshotForSSE(ctx, "inv-budget-live")
	if err != nil {
		t.Fatal(err)
	}
	if body := fetchFragment(t, srv, sid, "inv-budget-live"); !strings.Contains(body, "Waiting for the model.") {
		t.Fatalf("active investigation with no pending step should render Waiting for the model, got:\n%s", body)
	}

	if err := st.AccumulateTokens(ctx, "inv-budget-live", 501000, 1000); err != nil {
		t.Fatal(err)
	}
	if err := st.UpdateInvestigationStatus(ctx, "inv-budget-live", "paused"); err != nil {
		t.Fatal(err)
	}
	snapAfter, err := srv.snapshotForSSE(ctx, "inv-budget-live")
	if err != nil {
		t.Fatal(err)
	}
	if snapBefore == snapAfter {
		t.Fatalf("snapshot must change when budget counters/status change: %s", snapAfter)
	}
	for _, want := range []string{`"status":"paused"`, `"used_tokens":502000`} {
		if !strings.Contains(snapAfter, want) {
			t.Fatalf("budget snapshot missing %q: %s", want, snapAfter)
		}
	}

	body := fetchFragment(t, srv, sid, "inv-budget-live")
	for _, want := range []string{"Budget exhausted", "+ 500k tokens"} {
		if !strings.Contains(body, want) {
			t.Fatalf("paused budget fragment missing %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "Waiting for the model.") {
		t.Fatalf("paused budget fragment must replace the waiting placeholder:\n%s", body)
	}
}

// TestInvestigationActionContentNegotiation verifies the AJAX seam: an action
// POSTed by the in-page fetch engine (X-Requested-With: fetch) gets live
// fragments back (with the rejection flash rendered in place), while a classic
// browser POST still gets the 303 redirect (progressive enhancement).
func TestInvestigationActionContentNegotiation(t *testing.T) {
	srv, st := newTestServer(t) // availability disabled → continue is rejected before any loop call
	ctx := context.Background()
	if err := st.InsertInvestigation(ctx, store.Investigation{
		ID: "inv-neg", Goal: "diagnose router 502", Status: "active",
		CreatedBy: "operator", Model: "m", BaseURL: "https://llm.example/v1",
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.FinishInvestigation(ctx, "inv-neg", "aborted", `{"error":"llm http 502"}`); err != nil {
		t.Fatal(err)
	}
	sid, csrf, err := srv.sessions.issue(ctx, "admin", time.Hour)
	if err != nil {
		t.Fatal(err)
	}

	newPost := func(fetch bool) *httptest.ResponseRecorder {
		form := strings.NewReader("csrf=" + csrf + "&investigation_id=inv-neg&message=retry")
		req := httptest.NewRequest(http.MethodPost, "/investigations/continue", form)
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		if fetch {
			req.Header.Set("X-Requested-With", "fetch")
		}
		req.AddCookie(&http.Cookie{Name: cookieSession, Value: sid})
		req.AddCookie(&http.Cookie{Name: cookieCSRF, Value: csrf})
		rw := httptest.NewRecorder()
		srv.Routes().ServeHTTP(rw, req)
		return rw
	}

	// Classic browser POST → 303 to the detail page.
	plain := newPost(false)
	if plain.Code != http.StatusSeeOther {
		t.Fatalf("classic POST want 303, got %d body=%s", plain.Code, plain.Body.String())
	}
	if loc := plain.Header().Get("Location"); loc != "/investigations/inv-neg" {
		t.Fatalf("classic POST want redirect to detail, got %q", loc)
	}

	// fetch POST → 200 live fragments, with the rejection rendered in place.
	ajax := newPost(true)
	if ajax.Code != http.StatusOK {
		t.Fatalf("fetch POST want 200, got %d body=%s", ajax.Code, ajax.Body.String())
	}
	body := ajax.Body.String()
	if !strings.Contains(body, `id="investigation-status-fragment"`) {
		t.Fatalf("fetch POST should return live fragments, got:\n%s", body)
	}
	if strings.Contains(body, "<html") || strings.Contains(body, "data-active=") {
		t.Fatalf("fetch POST should return a fragment, not a full page:\n%s", body)
	}
	for _, want := range []string{"Continue blocked", "Continuation requires a configured LLM client"} {
		if !strings.Contains(body, want) {
			t.Fatalf("fetch POST fragment should render the rejection flash in place, missing %q:\n%s", want, body)
		}
	}
}

// TestInvestigationSSEPushesInitialAndBusStateChange verifies the SSE channel
// pushes an initial snapshot on connect and re-emits a state-change when the
// investigator Bus publishes an event — the push path that replaced the
// poll-only handler so a live update is no longer missed.
func TestInvestigationSSEPushesInitialAndBusStateChange(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	st, err := store.Open(ctx, filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	// nil LLM + a pending tool_call ⇒ EnsureProgress never spawns a worker, so
	// the test is deterministic and the Bus is the only state-change driver
	// (the safety re-snapshot ticker is 10s, well past the test window).
	loop := investigator.NewLoop(st, nil, nil, func(string) bool { return false }, func() []string { return nil }, 40, 500000,
		slog.New(slog.NewTextHandler(discardWriter{}, nil)))
	loop.SetBus(investigator.NewBus())
	srv, err := NewServer(st, nil, loop, NewInvestigatorAvailability(loop, ""), nil,
		AuthConfig{}, InstallConfig{}, slog.New(slog.NewTextHandler(discardWriter{}, nil)))
	if err != nil {
		t.Fatalf("new server: %v", err)
	}

	if err := st.InsertInvestigation(ctx, store.Investigation{
		ID: "inv-sse-push", Goal: "g", Status: "active",
		CreatedBy: "operator", Model: "m", BaseURL: "https://llm.example/v1",
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.InsertToolCall(ctx, store.ToolCallRow{
		ID: "tc-sse", InvestigationID: "inv-sse-push", Seq: 1,
		Tool: "collect", InputJSON: `{"host_id":"h"}`, Status: "pending",
	}); err != nil {
		t.Fatal(err)
	}
	sid, _, err := srv.sessions.issue(ctx, "admin", time.Hour)
	if err != nil {
		t.Fatal(err)
	}

	ts := httptest.NewServer(srv.Routes())
	t.Cleanup(ts.Close)

	reqCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	req, _ := http.NewRequestWithContext(reqCtx, http.MethodGet, ts.URL+"/investigations/events/inv-sse-push", nil)
	req.AddCookie(&http.Cookie{Name: cookieSession, Value: sid})
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("sse connect: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("sse status %d", resp.StatusCode)
	}

	events := make(chan string, 64)
	go func() {
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			if line := sc.Text(); strings.HasPrefix(line, "event: ") {
				events <- strings.TrimPrefix(line, "event: ")
			}
		}
		close(events)
	}()

	waitFor := func(name string, timeout time.Duration) {
		t.Helper()
		deadline := time.After(timeout)
		for {
			select {
			case ev, ok := <-events:
				if !ok {
					t.Fatalf("sse stream closed before %q", name)
				}
				if ev == name {
					return
				}
			case <-deadline:
				t.Fatalf("timed out waiting for sse %q", name)
			}
		}
	}

	// 1. Initial snapshot pushed on connect.
	waitFor("state-change", 3*time.Second)

	// 2. Change state, then publish a Bus event → handler re-emits state-change.
	if err := st.AddFinding(ctx, store.Finding{
		ID: "f-sse", InvestigationID: "inv-sse-push", Severity: "warn",
		Code: "x.y", Message: "m",
	}); err != nil {
		t.Fatal(err)
	}
	loop.Bus().Publish("inv-sse-push", investigator.EventFindingAdded, map[string]any{"id": "f-sse"})
	waitFor("state-change", 3*time.Second)
}

// TestInvestigationLiveEngineCannotWedge pins the two structural guarantees
// added after the live engine stuck on "Waiting for the model." for a fifth
// time. Both are string-asserted against the shipped hub.js because the
// project has no JS test runner — and the recurring failure was precisely that
// nothing guarded this wiring from being refactored away.
//
//  1. The fetch-stall wedge: every engine fetch must be abortable on a timeout,
//     so a never-settling request cannot leave the shared `refreshing` flag
//     true forever (which silently disables SSE push AND the backstop poll,
//     since both early-return on it). A separate watchdog backs this up.
//  2. The focus-freeze wedge: a region whose swap is skipped to preserve
//     operator focus must be tracked (`dirty`) and retried, instead of having
//     the fingerprint advanced past it — which froze it permanently once the
//     server stopped changing (e.g. the waiting→paused budget transition).
func TestInvestigationLiveEngineCannotWedge(t *testing.T) {
	js, err := os.ReadFile(filepath.Join("static", "hub.js"))
	if err != nil {
		t.Fatal(err)
	}
	src := string(js)

	// (1) Abortable fetch with a timeout, on both the fragment refresh and the
	// no-reload action POST.
	for _, want := range []string{
		"REFRESH_TIMEOUT_MS",
		"new AbortController()",
		"ctl.abort()",
		"signal: ctl ? ctl.signal : undefined",
	} {
		if !strings.Contains(src, want) {
			t.Fatalf("hub.js must bound fetches with an AbortController timeout; missing %q", want)
		}
	}
	if strings.Count(src, "signal: ctl ? ctl.signal : undefined") < 2 {
		t.Fatal("both the fragment refresh and the action POST must pass an abort signal")
	}

	// (1b) Independent watchdog that force-releases a stuck refresh — it must be
	// its own timer, since a stuck refresh() never re-enters past the guard.
	for _, want := range []string{
		"startWatchdog",
		"force-releasing stuck refresh",
		"refreshing = false",
	} {
		if !strings.Contains(src, want) {
			t.Fatalf("hub.js must have an independent refresh watchdog; missing %q", want)
		}
	}

	// (2) Skipped (focus-guarded) region swaps are tracked and retried, not
	// advanced past.
	for _, want := range []string{
		"var dirty = {}",
		"dirty[id] = true",
		"if (replaceRegion(id, doc)) { delete dirty[id]; }",
	} {
		if !strings.Contains(src, want) {
			t.Fatalf("hub.js must retry focus-skipped region swaps; missing %q", want)
		}
	}
}

// TestInvestigationLivePerRegionGate pins the per-region swap gate that fixes
// the flicker: each region is swapped only when ITS OWN data-frag-hash changed,
// not when a single global fingerprint moved (the old behavior reflowed all
// three regions — collapsing the timeline's <details>/scroll — on every token
// tick). String-asserted because the project has no JS runner.
func TestInvestigationLivePerRegionGate(t *testing.T) {
	js, err := os.ReadFile(filepath.Join("static", "hub.js"))
	if err != nil {
		t.Fatal(err)
	}
	src := string(js)
	// Per-region comparison on data-frag-hash, replacing the old global gate.
	for _, want := range []string{
		"data-frag-hash",
		"var regionChanged =",
		"if (!regionChanged && !dirty[id]) return;",
	} {
		if !strings.Contains(src, want) {
			t.Fatalf("hub.js must gate swaps per region by data-frag-hash; missing %q", want)
		}
	}
	// The dirty[] focus-retry must still bypass the per-region gate (skip only
	// when clean AND unchanged), or a focus-skipped region freezes — the wedge
	// class TestInvestigationLiveEngineCannotWedge guards.
	if !strings.Contains(src, "if (replaceRegion(id, doc)) { delete dirty[id]; }") {
		t.Fatal("hub.js must retry focus-skipped regions regardless of per-region hash equality")
	}
	// Status header churns only on the budget readouts — patch them in place
	// instead of repainting the whole header on every token tick.
	for _, want := range []string{"patchStatusInPlace", "budgetSig"} {
		if !strings.Contains(src, want) {
			t.Fatalf("hub.js must update the status budget readouts in place; missing %q", want)
		}
	}
}

// TestDirectTheModelTriggerAndModal pins the right-panel redesign: the cramped
// inline hypothesis form is replaced by a compact trigger that opens a modal
// built in hub.js. Guards both the trigger (server-rendered, carries a fresh
// CSRF) and the modal wiring from silent removal.
func TestDirectTheModelTriggerAndModal(t *testing.T) {
	srv, st := newTestServer(t)
	ctx := context.Background()
	if err := st.InsertInvestigation(ctx, store.Investigation{
		ID: "inv-hyp-ui", Goal: "g", Status: "active",
		CreatedBy: "operator", Model: "m", BaseURL: "https://llm.example/v1",
	}); err != nil {
		t.Fatal(err)
	}
	sid, csrf, err := srv.sessions.issue(ctx, "admin", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	body := fetchFragment(t, srv, sid, "inv-hyp-ui")
	for _, want := range []string{`data-hyp-open`, `data-inv-id="inv-hyp-ui"`, `data-csrf="` + csrf + `"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("active side fragment should render the Direct-the-model trigger, missing %q", want)
		}
	}
	if strings.Contains(body, `<textarea name="claim"`) {
		t.Fatalf("the old cramped inline hypothesis form should be gone:\n%s", body)
	}
	// The modal + its own POST live in hub.js (CSP forbids external scripts).
	js, err := os.ReadFile(filepath.Join("static", "hub.js"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"data-hyp-open", "hyp-overlay", "'/investigations/hypothesis'"} {
		if !strings.Contains(string(js), want) {
			t.Fatalf("hub.js must build the Direct-the-model modal; missing %q", want)
		}
	}
}

// fetchFragment GETs the investigation live fragments as the in-page engine
// would and returns the body, failing the test on a non-200.
func fetchFragment(t *testing.T, srv *Server, sid, id string) string {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/investigations/fragments/"+id, nil)
	req.Header.Set("X-Requested-With", "fetch")
	req.AddCookie(&http.Cookie{Name: cookieSession, Value: sid})
	rw := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rw, req)
	if rw.Code != http.StatusOK {
		t.Fatalf("fragment GET want 200, got %d body=%s", rw.Code, rw.Body.String())
	}
	return rw.Body.String()
}
