package investigator

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/vasyakrg/recon/internal/hub/llm"
)

const (
	// defaultHistoryKeepRecentResults is how many of the most recent tool
	// results stay verbatim on the wire; older bulky probe results are demoted
	// to one-line pointers (Task 3).
	defaultHistoryKeepRecentResults = 6
	// defaultHistoryDemoteMinBytes is the smallest tool-result body worth
	// demoting; smaller results are already cheap and the pointer would not
	// save enough to bother.
	defaultHistoryDemoteMinBytes = 1024
)

// historyPreserveTools are tool results that MUST stay verbatim regardless of
// age — they are load-bearing for the investigation's audit/decision trail and
// carry no re-readable artifact behind a task_id.
var historyPreserveTools = map[string]bool{
	"add_finding":  true,
	"ask_operator": true,
	"mark_done":    true,
}

// demoteStats reports what demoteAgedToolResults did, for verbose logging. It
// never carries demoted bodies.
type demoteStats struct {
	MessagesTotal int
	Demoted       int
	BytesBefore   int
	BytesAfter    int
	KeepRecent    int
}

// demoteAgedToolResults returns a copy of the wire-format history in which the
// Content of aged, bulky probe tool results is replaced by a compact pointer
// that names the task_id(s) so the model can re-read via get_full_result /
// search_artifact. The most recent keepRecent tool results stay verbatim, as do
// the system prompt, user goal, COMPACT_STATE / system_summary, system notes,
// and any add_finding / ask_operator / mark_done result.
//
// It never mutates the input: a fresh slice is returned and only the Content of
// demoted entries differs. The stored message history is untouched, so
// get_full_result and the audit trail stay complete (the durable memory +
// notebook + citation rule 3 keep findings re-derivable).
func demoteAgedToolResults(msgs []llm.Message, keepRecent, minBytes int) ([]llm.Message, demoteStats) {
	if keepRecent <= 0 {
		keepRecent = defaultHistoryKeepRecentResults
	}
	if minBytes <= 0 {
		minBytes = defaultHistoryDemoteMinBytes
	}
	stats := demoteStats{MessagesTotal: len(msgs), KeepRecent: keepRecent}

	// Map each tool result's call id -> tool name via the assistant tool_calls
	// that precede it, so we can tell a collect result from an add_finding one.
	toolByCallID := map[string]string{}
	toolResultIdx := make([]int, 0, len(msgs))
	for i, m := range msgs {
		switch m.Role {
		case "assistant":
			for _, tc := range m.ToolCalls {
				if tc.ID != "" {
					toolByCallID[tc.ID] = tc.Function.Name
				}
			}
		case "tool":
			toolResultIdx = append(toolResultIdx, i)
		}
	}

	// The last keepRecent tool results stay verbatim. cutoff is the index in
	// toolResultIdx; results before it are demotion candidates.
	cutoff := len(toolResultIdx) - keepRecent
	if cutoff < 0 {
		cutoff = 0
	}
	demotable := map[int]bool{}
	for k := 0; k < cutoff; k++ {
		demotable[toolResultIdx[k]] = true
	}

	out := make([]llm.Message, len(msgs))
	copy(out, msgs)
	for i := range out {
		if out[i].Role != "tool" {
			continue
		}
		if !demotable[i] {
			continue
		}
		if len(out[i].Content) < minBytes {
			continue
		}
		tool := toolByCallID[out[i].ToolCallID]
		if historyPreserveTools[tool] {
			continue
		}
		pointer, ok := demotionPointer(tool, out[i].Content)
		if !ok {
			continue
		}
		stats.BytesBefore += len(out[i].Content)
		stats.BytesAfter += len(pointer)
		stats.Demoted++
		out[i].Content = pointer
	}
	return out, stats
}

// demotionPointer builds the one-line replacement for an aged tool result. It
// returns ok=false when the result carries no task_id (no re-read path), so the
// caller leaves such results untouched.
func demotionPointer(tool, content string) (string, bool) {
	var envelope struct {
		Data json.RawMessage `json:"data"`
	}
	if json.Unmarshal([]byte(content), &envelope) != nil || len(envelope.Data) == 0 {
		return "", false
	}
	var d struct {
		Tasks []struct {
			TaskID    string `json:"task_id"`
			Collector string `json:"collector"`
			HostID    string `json:"host_id"`
			Status    string `json:"status"`
		} `json:"tasks"`
		TaskID string `json:"task_id"`
	}
	if json.Unmarshal(envelope.Data, &d) != nil {
		return "", false
	}
	const reread = " — full result elided to save context; call get_full_result(task_id) / search_artifact(task_id, pattern) to re-read"
	if tool == "" {
		tool = "probe"
	}

	if len(d.Tasks) == 1 {
		t := d.Tasks[0]
		return fmt.Sprintf("RESULT tool=%s task_id=%s collector=%s host=%s status=%s%s",
			tool, t.TaskID, orNA(t.Collector), orNA(t.HostID), orNA(t.Status), reread), true
	}
	if len(d.Tasks) > 1 {
		ids := make([]string, 0, len(d.Tasks))
		for _, t := range d.Tasks {
			if t.TaskID != "" {
				ids = append(ids, t.TaskID)
			}
		}
		if len(ids) == 0 {
			return "", false
		}
		collector := d.Tasks[0].Collector
		return fmt.Sprintf("RESULT tool=%s task_ids=%s collector=%s hosts=%d%s",
			tool, joinCapped(ids, 8), orNA(collector), len(d.Tasks), reread), true
	}
	if d.TaskID != "" {
		return fmt.Sprintf("RESULT tool=%s task_id=%s%s", tool, d.TaskID, reread), true
	}
	return "", false
}

func orNA(s string) string {
	if s == "" {
		return "?"
	}
	return s
}

// joinCapped joins up to n ids with commas, appending "(+k more)" when the list
// is longer so the pointer stays short on huge fleet surveys.
func joinCapped(ids []string, n int) string {
	if len(ids) <= n {
		return strings.Join(ids, ",")
	}
	return strings.Join(ids[:n], ",") + fmt.Sprintf("(+%d more)", len(ids)-n)
}
