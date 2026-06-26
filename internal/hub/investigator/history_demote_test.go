package investigator

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/vasyakrg/recon/internal/hub/llm"
)

// collectResultBody returns a realistic, bulky collect tool-result body for one
// task so demotion has something worth eliding.
func collectResultBody(taskID, host string) string {
	body, _ := json.Marshal(ToolResult{OK: true, Data: map[string]any{
		"tasks": []map[string]any{{
			"task_id":   taskID,
			"host_id":   host,
			"collector": "journal",
			"status":    "done",
			"summary":   strings.Repeat("x", 4000),
		}},
	}})
	return string(body)
}

func assistantCall(callID, tool string) llm.Message {
	return llm.Message{Role: "assistant", Content: "probe", ToolCalls: []llm.ToolCall{{
		ID: callID, Type: "function", Function: llm.ToolCallInvocation{Name: tool},
	}}}
}

func TestDemoteAgedToolResultsKeepsRecentVerbatim(t *testing.T) {
	msgs := []llm.Message{
		{Role: "system", Content: "system prompt"},
		{Role: "user", Content: "goal"},
	}
	// Six aged collect results, then two recent ones (keepRecent=2).
	bodies := map[string]string{}
	for i := 0; i < 8; i++ {
		id := "call" + string(rune('a'+i))
		body := collectResultBody("task"+string(rune('a'+i)), "h"+string(rune('a'+i)))
		bodies[id] = body
		msgs = append(msgs, assistantCall(id, "collect"))
		msgs = append(msgs, llm.Message{Role: "tool", Content: body, ToolCallID: id})
	}

	out, stats := demoteAgedToolResults(msgs, 2, 512)
	if stats.Demoted != 6 {
		t.Fatalf("want 6 aged results demoted, got %d", stats.Demoted)
	}
	if stats.BytesAfter >= stats.BytesBefore {
		t.Fatalf("demotion must shrink bytes: before=%d after=%d", stats.BytesBefore, stats.BytesAfter)
	}

	// Input must be untouched (view-only contract).
	for _, m := range msgs {
		if m.Role == "tool" && strings.HasPrefix(m.Content, "RESULT ") {
			t.Fatalf("input history was mutated — demotion must be view-only")
		}
	}

	// Walk the output: count verbatim vs pointer tool results.
	verbatim, pointers := 0, 0
	for _, m := range out {
		if m.Role != "tool" {
			continue
		}
		if strings.HasPrefix(m.Content, "RESULT ") {
			pointers++
			if !strings.Contains(m.Content, "get_full_result") || !strings.Contains(m.Content, "task_id=") {
				t.Fatalf("pointer must steer re-read with task_id: %q", m.Content)
			}
		} else {
			verbatim++
		}
	}
	if pointers != 6 || verbatim != 2 {
		t.Fatalf("want 6 pointers + 2 verbatim, got %d/%d", pointers, verbatim)
	}
}

func TestDemoteAgedToolResultsPreservesFindingsAndAsks(t *testing.T) {
	msgs := []llm.Message{
		{Role: "system", Content: "system prompt"},
		{Role: "user", Content: "goal"},
		assistantCall("c1", "add_finding"),
		{Role: "tool", Content: collectResultBody("ignored", "h1"), ToolCallID: "c1"},
		assistantCall("c2", "ask_operator"),
		{Role: "tool", Content: collectResultBody("ignored2", "h2"), ToolCallID: "c2"},
		assistantCall("c3", "collect"),
		{Role: "tool", Content: collectResultBody("taskZ", "h3"), ToolCallID: "c3"},
		// Two recent fillers so the above collect is "aged" under keepRecent=1.
		assistantCall("c4", "collect"),
		{Role: "tool", Content: collectResultBody("taskY", "h4"), ToolCallID: "c4"},
	}
	out, stats := demoteAgedToolResults(msgs, 1, 512)
	// Only the aged collect (c3) is eligible; add_finding + ask_operator stay.
	if stats.Demoted != 1 {
		t.Fatalf("only the aged collect should demote, got %d", stats.Demoted)
	}
	findContent := func(callID string) string {
		for _, m := range out {
			if m.Role == "tool" && m.ToolCallID == callID {
				return m.Content
			}
		}
		return ""
	}
	if strings.HasPrefix(findContent("c1"), "RESULT ") {
		t.Fatalf("add_finding result must never be demoted")
	}
	if strings.HasPrefix(findContent("c2"), "RESULT ") {
		t.Fatalf("ask_operator result must never be demoted")
	}
	if !strings.HasPrefix(findContent("c3"), "RESULT ") {
		t.Fatalf("aged collect result should be demoted: %q", findContent("c3"))
	}
}

func TestDemoteAgedToolResultsSkipsSmallBodies(t *testing.T) {
	small, _ := json.Marshal(ToolResult{OK: true, Data: map[string]any{
		"tasks": []map[string]any{{"task_id": "t1", "host_id": "h1", "collector": "system_info", "status": "done"}},
	}})
	msgs := []llm.Message{
		{Role: "system", Content: "system"},
		assistantCall("c1", "collect"),
		{Role: "tool", Content: string(small), ToolCallID: "c1"},
		assistantCall("c2", "collect"),
		{Role: "tool", Content: collectResultBody("t2", "h2"), ToolCallID: "c2"},
		assistantCall("c3", "collect"),
		{Role: "tool", Content: collectResultBody("t3", "h3"), ToolCallID: "c3"},
	}
	// keepRecent=1 makes c1 and c2 aged; c1's body is below minBytes so only c2 demotes.
	_, stats := demoteAgedToolResults(msgs, 1, 1024)
	if stats.Demoted != 1 {
		t.Fatalf("small aged body should be skipped; want 1 demoted, got %d", stats.Demoted)
	}
}
