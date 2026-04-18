package investigator

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/vasyakrg/recon/internal/hub/llm"
	"github.com/vasyakrg/recon/internal/hub/runner"
	"github.com/vasyakrg/recon/internal/hub/store"
)

// Loop drives the step-by-step investigation: one LLM call → one tool_call
// → operator decision → execute → tool_result → next LLM call. State lives
// in store; this struct is process-global, one instance per hub.
type Loop struct {
	store  *store.Store
	llm    *llm.Client
	runner *runner.Runner
	online func(string) bool
	agents func() []string
	log    *slog.Logger

	maxSteps  int
	maxTokens int

	mu      sync.Mutex
	running map[string]bool // investigationID — prevents concurrent advance
}

func NewLoop(st *store.Store, llmC *llm.Client, run *runner.Runner,
	online func(string) bool, agents func() []string,
	maxSteps, maxTokens int, log *slog.Logger) *Loop {
	return &Loop{
		store: st, llm: llmC, runner: run,
		online: online, agents: agents,
		maxSteps: maxSteps, maxTokens: maxTokens,
		log:     log,
		running: map[string]bool{},
	}
}

// Start creates a new investigation row, persists the system prompt + user
// goal as the first two messages, and triggers the first LLM call.
func (l *Loop) Start(ctx context.Context, goal, createdBy string) (string, error) {
	if goal == "" {
		return "", errors.New("goal is empty")
	}
	if l.llm == nil {
		return "", errors.New("LLM client not configured (set RECON_LLM_API_KEY)")
	}
	id := newInvestigationID()
	inv := store.Investigation{
		ID:        id,
		Goal:      goal,
		Status:    "active",
		CreatedBy: createdBy,
		Model:     l.llm.Model(),
		BaseURL:   "configured",
	}
	if err := l.store.InsertInvestigation(ctx, inv); err != nil {
		return "", err
	}
	system := BuildSystemPrompt(goal, l.llm.Model(), time.Now(), l.maxSteps, l.maxTokens)
	if _, err := l.store.AppendMessage(ctx, store.Message{
		InvestigationID: id, Role: "system", Content: system,
	}); err != nil {
		return "", err
	}
	if _, err := l.store.AppendMessage(ctx, store.Message{
		InvestigationID: id, Role: "user", Content: goal,
	}); err != nil {
		return "", err
	}
	// Kick off the first LLM call asynchronously — operator polls the page.
	l.spawn(id)
	return id, nil
}

// spawn launches advance() in a fresh background goroutine. The investigator
// loop intentionally outlives the HTTP request that triggered it — operator
// closes the browser tab and returns later to a partially-done investigation.
func (l *Loop) spawn(id string) {
	//nolint:gosec // G118: see godoc on spawn — fresh ctx is the design.
	go l.advance(context.Background(), id)
}

// Decide records an operator decision on a pending tool call and resumes
// the loop. Decision: "approve" | "skip" | "end".
func (l *Loop) Decide(ctx context.Context, investigationID, decision, decidedBy string) error {
	pending, err := l.store.PendingToolCall(ctx, investigationID)
	if err != nil {
		return err
	}
	if pending == nil {
		return errors.New("no pending tool call")
	}
	switch decision {
	case "approve":
		if err := l.store.UpdateToolCall(ctx, pending.ID, "approved", decidedBy, "", ""); err != nil {
			return err
		}
	case "skip":
		// Record skip and synthesize a tool message so the LLM sees a result.
		skipResult := ToolResult{OK: false, Error: "operator skipped this step"}
		body, _ := json.Marshal(skipResult)
		if err := l.store.UpdateToolCall(ctx, pending.ID, "skipped", decidedBy, "", string(body)); err != nil {
			return err
		}
		if _, err := l.store.AppendMessage(ctx, store.Message{
			InvestigationID: investigationID, Role: "tool",
			Content: string(body), ToolCallID: sql.NullString{String: pending.ID, Valid: true},
		}); err != nil {
			return err
		}
	case "end":
		if err := l.store.UpdateToolCall(ctx, pending.ID, "aborted", decidedBy, "", ""); err != nil {
			return err
		}
		return l.store.FinishInvestigation(ctx, investigationID, "aborted", `{"summary":"operator ended"}`)
	default:
		return fmt.Errorf("unknown decision %q", decision)
	}
	l.spawn(investigationID)
	return nil
}

// advance runs one LLM step. It is serialized per investigation via
// l.running so an operator who clicks Approve twice does not double-fire.
func (l *Loop) advance(ctx context.Context, investigationID string) {
	l.mu.Lock()
	if l.running[investigationID] {
		l.mu.Unlock()
		return
	}
	l.running[investigationID] = true
	l.mu.Unlock()
	defer func() {
		l.mu.Lock()
		delete(l.running, investigationID)
		l.mu.Unlock()
	}()

	for {
		ok, err := l.step(ctx, investigationID)
		if err != nil {
			l.log.Error("investigator step", "investigation_id", investigationID, "err", err)
			_ = l.store.FinishInvestigation(ctx, investigationID, "aborted",
				fmt.Sprintf(`{"error":%q}`, err.Error()))
			return
		}
		if !ok {
			// Either we put a pending tool call (waiting on operator) or the
			// investigation reached a terminal state.
			return
		}
	}
}

// step does one full turn: call LLM → parse tool call → either execute it
// (approved tools) or persist as pending. Returns true when the loop should
// continue immediately (e.g. a pre-approved tool was executed inline).
func (l *Loop) step(ctx context.Context, investigationID string) (bool, error) {
	inv, err := l.store.GetInvestigation(ctx, investigationID)
	if err != nil {
		return false, err
	}
	if inv.Status == "done" || inv.Status == "aborted" {
		return false, nil
	}
	if inv.TotalToolCalls >= l.maxSteps {
		_ = l.store.FinishInvestigation(ctx, investigationID, "aborted",
			fmt.Sprintf(`{"reason":"max_steps_exceeded","budget":%d}`, l.maxSteps))
		return false, nil
	}
	if inv.TotalPromptTokens+inv.TotalCompletionTokens >= l.maxTokens {
		_ = l.store.FinishInvestigation(ctx, investigationID, "aborted",
			fmt.Sprintf(`{"reason":"max_tokens_exceeded","budget":%d}`, l.maxTokens))
		return false, nil
	}

	// If there is already a pending tool call, the operator has not decided.
	pending, err := l.store.PendingToolCall(ctx, investigationID)
	if err != nil {
		return false, err
	}
	if pending != nil {
		// Pending may also be 'approved' awaiting execution — handle here.
		// PendingToolCall only returns status='pending', so this branch
		// means the operator has not acted yet.
		return false, nil
	}

	// Find the most recent approved (but not executed) tool call to run.
	approved, err := l.lastApproved(ctx, investigationID)
	if err != nil {
		return false, err
	}
	if approved != nil {
		if err := l.executeApproved(ctx, investigationID, approved); err != nil {
			return false, err
		}
		return true, nil
	}

	// Otherwise: time to ask the LLM for the next move.
	return l.callLLM(ctx, inv)
}

func (l *Loop) callLLM(ctx context.Context, inv store.Investigation) (bool, error) {
	msgs, err := l.store.ListMessages(ctx, inv.ID, false)
	if err != nil {
		return false, err
	}
	pbMsgs := make([]llm.Message, 0, len(msgs))
	for _, m := range msgs {
		switch m.Role {
		case "system", "user":
			pbMsgs = append(pbMsgs, llm.Message{Role: m.Role, Content: m.Content})
		case "assistant":
			// (C1) Reconstruct assistant message with the SAME tool_calls
			// the LLM emitted, otherwise the next-turn 'tool' message has
			// no preceding assistant tool_call to anchor on, and the
			// provider rejects the conversation with "tool_call_id not
			// found".
			am := llm.Message{Role: "assistant", Content: m.Content}
			if m.ToolCallsJSON.Valid && m.ToolCallsJSON.String != "" {
				var tcs []llm.ToolCall
				if err := json.Unmarshal([]byte(m.ToolCallsJSON.String), &tcs); err == nil {
					am.ToolCalls = tcs
				}
			}
			pbMsgs = append(pbMsgs, am)
		case "system_note":
			// Encoded as a user message with a SYSTEM NOTE prefix.
			pbMsgs = append(pbMsgs, llm.Message{Role: "user", Content: "SYSTEM NOTE: " + m.Content})
		case "tool":
			tcID := ""
			if m.ToolCallID.Valid {
				tcID = m.ToolCallID.String
			}
			pbMsgs = append(pbMsgs, llm.Message{Role: "tool", Content: m.Content, ToolCallID: tcID})
		}
	}

	resp, err := l.llm.Chat(ctx, llm.ChatRequest{
		Messages:    pbMsgs,
		Tools:       Tools(),
		ToolChoice:  "required",
		Temperature: 0,
		MaxTokens:   4096,
	})
	if err != nil {
		return false, fmt.Errorf("llm chat: %w", err)
	}
	_ = l.store.AccumulateTokens(ctx, inv.ID, resp.Usage.PromptTokens, resp.Usage.CompletionTokens)

	choice := resp.Choices[0].Message

	// (C1) Store assistant content (rationale) AS-IS in `content`, and the
	// tool_calls list (after one-tool-per-turn enforcement below) in a
	// separate column. ListMessages reassembles both on the next turn.
	keptCalls := choice.ToolCalls
	if len(keptCalls) > 1 {
		keptCalls = keptCalls[:1]
	}
	var toolCallsJSON sql.NullString
	if len(keptCalls) > 0 {
		body, _ := json.Marshal(keptCalls)
		toolCallsJSON = sql.NullString{String: string(body), Valid: true}
	}
	if _, err := l.store.AppendMessage(ctx, store.Message{
		InvestigationID: inv.ID, Role: "assistant",
		Content: choice.Content, ToolCallsJSON: toolCallsJSON,
	}); err != nil {
		return false, err
	}

	if len(choice.ToolCalls) == 0 {
		// LLM violated the contract — synthesize a system_note nudging it.
		_, _ = l.store.AppendMessage(ctx, store.Message{
			InvestigationID: inv.ID, Role: "system_note",
			Content: "Your previous response did not include a tool_call. Per the rules you MUST emit exactly one tool_call per turn. Use ask_operator if you need to ask a question.",
		})
		return true, nil
	}

	// Enforce one-tool-call-per-turn — accept the first, drop the rest.
	first := choice.ToolCalls[0]
	if len(choice.ToolCalls) > 1 {
		_, _ = l.store.AppendMessage(ctx, store.Message{
			InvestigationID: inv.ID, Role: "system_note",
			Content: fmt.Sprintf("You returned %d tool_calls; only the first (%s) was kept. Stick to ONE per turn.", len(choice.ToolCalls), first.Function.Name),
		})
	}

	// Persist as pending. Auto-tools (no host-touch, no findings, no
	// finalize) are pre-approved so the operator does not have to click
	// through trivial discovery steps.
	autoApprove := isAutoTool(first.Function.Name)
	status := "pending"
	if autoApprove {
		status = "approved"
	}
	if err := l.store.InsertToolCall(ctx, store.ToolCallRow{
		ID: first.ID, InvestigationID: inv.ID, Seq: nextSeq(ctx, l.store, inv.ID),
		Tool: first.Function.Name, InputJSON: first.Function.Arguments,
		Rationale: choice.Content, Status: status,
	}); err != nil {
		return false, err
	}
	_ = l.store.IncrementToolCalls(ctx, inv.ID)

	if autoApprove {
		return true, nil // execute immediately
	}
	return false, nil // wait for operator
}

// executeApproved runs the named tool, persists result, appends the tool
// message and marks the call executed. For mark_done / ask_operator it also
// updates the investigation status accordingly.
func (l *Loop) executeApproved(ctx context.Context, investigationID string, tc *store.ToolCallRow) error {
	env := HandlerEnv{
		Store:           l.store,
		Runner:          l.runner,
		Online:          l.online,
		OnlineAgents:    l.agents,
		InvestigationID: investigationID,
		ArtifactDir:     "", // set by runner when needed
	}

	var result ToolResult
	taskID := ""
	switch tc.Tool {
	case "collect":
		exec, err := PrepareCollect(ctx, env, tc.InputJSON)
		if err != nil {
			result = errResult(err)
		} else {
			waitForTasks(ctx, l.store, exec.TaskIDs, 60*time.Second)
			result = SummarizeTasks(ctx, env, exec.TaskIDs)
			if len(exec.TaskIDs) > 0 {
				taskID = exec.TaskIDs[0]
			}
		}
	case "collect_batch":
		exec, err := PrepareCollectBatch(ctx, env, tc.InputJSON)
		if err != nil {
			result = errResult(err)
		} else {
			waitForTasks(ctx, l.store, exec.TaskIDs, 120*time.Second)
			result = SummarizeTasks(ctx, env, exec.TaskIDs)
			taskID = strings.Join(exec.TaskIDs, ",")
		}
	default:
		result = Dispatch(ctx, env, tc.Tool, tc.InputJSON)
	}
	resultBytes, _ := json.Marshal(result)

	if err := l.store.UpdateToolCall(ctx, tc.ID, "executed", "auto", taskID, string(resultBytes)); err != nil {
		return err
	}
	if _, err := l.store.AppendMessage(ctx, store.Message{
		InvestigationID: investigationID, Role: "tool",
		Content: string(resultBytes), ToolCallID: sql.NullString{String: tc.ID, Valid: true},
	}); err != nil {
		return err
	}

	switch tc.Tool {
	case "mark_done":
		var args struct {
			Summary json.RawMessage `json:"summary"`
		}
		_ = json.Unmarshal([]byte(tc.InputJSON), &args)
		_ = l.store.FinishInvestigation(ctx, investigationID, "done", string(args.Summary))
	case "ask_operator":
		_ = l.store.UpdateInvestigationStatus(ctx, investigationID, "waiting")
	}
	return nil
}

// lastApproved returns the most recent tool_call with status='approved',
// nil if none.
func (l *Loop) lastApproved(ctx context.Context, investigationID string) (*store.ToolCallRow, error) {
	tcs, err := l.store.ListToolCalls(ctx, investigationID)
	if err != nil {
		return nil, err
	}
	for i := len(tcs) - 1; i >= 0; i-- {
		if tcs[i].Status == "approved" {
			tc := tcs[i]
			return &tc, nil
		}
	}
	return nil, nil
}

// isAutoTool returns true ONLY for pure-inventory tools that read DB rows
// already cached on the hub: no host I/O, no artifact reads, no findings
// created, no data sent to the LLM beyond a small in-memory listing. PROJECT.md
// §7.2 requires operator approval per step; these three are exempted because
// they merely surface what the operator already sees on /hosts and
// /collectors pages — clicking through them would be pure noise.
//
// Everything else (search_artifact, get_full_result, compare_across_hosts,
// add_finding, collect*, ask_operator, mark_done) goes through the operator.
// In particular: search_artifact + get_full_result are gated because they
// move file/result content into the LLM context, i.e. to a third-party
// provider — operator must consent.
func isAutoTool(name string) bool {
	switch name {
	case "list_hosts", "list_collectors", "describe_collector":
		return true
	}
	return false
}

// waitForTasks polls for terminal state on every taskID up to timeout. The
// runner's watchRunCompletion writes the terminal status; we just observe.
func waitForTasks(ctx context.Context, st *store.Store, ids []string, timeout time.Duration) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		allDone := true
		for _, id := range ids {
			done, err := taskTerminal(ctx, st, id)
			if err != nil || !done {
				allDone = false
				break
			}
		}
		if allDone {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(200 * time.Millisecond):
		}
	}
}

func taskTerminal(ctx context.Context, st *store.Store, id string) (bool, error) {
	t, err := st.GetTask(ctx, id)
	if err != nil {
		return false, err
	}
	switch t.Status {
	case "ok", "error", "timeout", "canceled", "undeliverable":
		return true, nil
	}
	return false, nil
}

func nextSeq(ctx context.Context, st *store.Store, investigationID string) int {
	tcs, _ := st.ListToolCalls(ctx, investigationID)
	return len(tcs) + 1
}

func newInvestigationID() string {
	var b [6]byte
	_, _ = rand.Read(b[:])
	return "inv_" + hex.EncodeToString(b[:])
}
