//go:build eval

// This is the OFFLINE, NON-DETERMINISTIC differential-methodology eval (HF-eval-b).
// It drives a REAL LLM and is deliberately excluded from CI: it is gated behind
// the `eval` build tag, so the normal `go test ./...` (per RULES.md, in Docker)
// never compiles or runs it. Run it manually as a periodic check:
//
//	RECON_LLM_API_KEY=... RECON_LLM_MODEL=anthropic/claude-sonnet-4.5 \
//	  go test -tags eval ./internal/hub/investigator/ -run TestDifferentialMethodologyEval -v
//
// It feeds the inv_a00000000002 incident shape — a kernel artifact_index whose
// loudest cluster is a 21k-line TPM storm and whose rare clusters are NIC/link
// flaps — and asserts the methodology (prompt rule 14): the model must consider
// the network/hardware class and must NOT mark_done while it is unchecked. A
// live LLM is nondeterministic, so it asserts a PASS-RATE over N runs, not exact
// output.
package investigator

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/vasyakrg/recon/internal/hub/llm"
	"github.com/vasyakrg/recon/internal/hub/logtriage"
)

const (
	evalRunsPerScenario = 5
	evalPassThreshold   = 4 // >= 4/5 runs must show the differential discipline
	evalMaxTurns        = 6
)

// networkKeywords mark that the model is pursuing the (rare) network/hardware
// class rather than anchoring on the loudest TPM cluster.
var networkKeywords = []string{
	"nic", "link", "carrier", "eno1", "eno", "bond", "ethernet",
	"network", "e1000", "igb", "ixgbe", "phy", "mii",
}

func mentionsNetwork(s string) bool {
	s = strings.ToLower(s)
	for _, k := range networkKeywords {
		if strings.Contains(s, k) {
			return true
		}
	}
	return false
}

type evalScenario struct {
	name string
	// initialCollect is the collect tool result the model sees up front.
	initialCollect string
	// onFullResult is what get_full_result returns. In the truncated scenario the
	// initial inline index is collapsed to the TPM headline, so the NIC clusters
	// live ONLY here — exercising the HF1 "_index_truncated → get_full_result"
	// rule.
	onFullResult string
}

func TestDifferentialMethodologyEval(t *testing.T) {
	if strings.TrimSpace(os.Getenv("RECON_LLM_API_KEY")) == "" {
		t.Skip("RECON_LLM_API_KEY unset; the offline differential eval is a manual, non-CI check")
	}
	client := newEvalClient(t)

	full := []logtriage.ArtifactIndex{kernelIndex(true)}
	collapsed := []logtriage.ArtifactIndex{kernelIndex(false)}
	scenarios := []evalScenario{
		{
			name:           "full_index",
			initialCollect: collectResultJSON(full, false),
			onFullResult:   collectResultJSON(full, false),
		},
		{
			name:           "truncated_index",
			initialCollect: collectResultJSON(collapsed, true),
			onFullResult:   collectResultJSON(full, false),
		},
	}

	for _, sc := range scenarios {
		passes := 0
		for i := 0; i < evalRunsPerScenario; i++ {
			ok, summary := runDifferentialEval(t, client, sc)
			t.Logf("[%s run %d/%d] pass=%v — %s", sc.name, i+1, evalRunsPerScenario, ok, summary)
			if ok {
				passes++
			}
		}
		if passes < evalPassThreshold {
			t.Errorf("scenario %q: %d/%d runs showed the differential discipline, want >= %d",
				sc.name, passes, evalRunsPerScenario, evalPassThreshold)
		}
	}
}

func newEvalClient(t *testing.T) *llm.Client {
	t.Helper()
	base := os.Getenv("RECON_LLM_BASE_URL")
	if base == "" {
		base = "https://openrouter.ai/api/v1"
	}
	model := os.Getenv("RECON_LLM_MODEL")
	if model == "" {
		model = "anthropic/claude-sonnet-4.5"
	}
	insecure := os.Getenv("RECON_LLM_ALLOW_INSECURE_HTTP") == "true"
	c, err := llm.NewFromEnv(base, model, "RECON_LLM_API_KEY", insecure, "recon-differential-eval", "recon-differential-eval")
	if err != nil {
		t.Fatalf("llm client: %v", err)
	}
	return c
}

// runDifferentialEval seeds the incident conversation and drives up to
// evalMaxTurns. A run PASSES when the model considers the network/hardware class
// (in any rationale or tool argument) before — or instead of — concluding, and
// FAILS when it calls mark_done while that high-prior class is still unconsidered
// (the original anchoring failure).
func runDifferentialEval(t *testing.T, client *llm.Client, sc evalScenario) (bool, string) {
	t.Helper()
	ctx := context.Background()

	goal := "Host host-x hung and became unresponsive on the network. Find the root cause."
	msgs := []llm.Message{
		{Role: "system", Content: BuildSystemPrompt(goal, client.Model(), time.Now().UTC(), 40, 500_000)},
		{Role: "assistant", Content: "Pulling the kernel ring to triage what brought the host down.",
			ToolCalls: []llm.ToolCall{{ID: "call_seed", Type: "function", Function: llm.ToolCallInvocation{
				Name: "collect", Arguments: `{"collector":"journal_tail","host_id":"host-x","params":{"kernel":"true"}}`,
			}}}},
		{Role: "tool", ToolCallID: "call_seed", Name: "collect", Content: sc.initialCollect},
	}

	networkConsidered := false
	for turn := 0; turn < evalMaxTurns; turn++ {
		resp, err := client.Chat(ctx, llm.ChatRequest{
			Model: client.Model(), Messages: msgs, Tools: Tools(),
			ToolChoice: "required", Temperature: 0, MaxTokens: 1024,
		})
		if err != nil {
			return false, "chat error: " + err.Error()
		}
		if len(resp.Choices) == 0 {
			return false, "no choices returned"
		}
		m := resp.Choices[0].Message
		if mentionsNetwork(m.Content) {
			networkConsidered = true
		}
		if len(m.ToolCalls) == 0 {
			return false, "assistant returned no tool_call (protocol violation)"
		}
		tc := m.ToolCalls[0]
		if mentionsNetwork(tc.Function.Arguments) {
			networkConsidered = true
		}
		msgs = append(msgs, llm.Message{Role: "assistant", Content: m.Content, ToolCalls: []llm.ToolCall{tc}})

		if tc.Function.Name == "mark_done" {
			return networkConsidered, fmt.Sprintf("mark_done at turn %d (network considered=%v)", turn+1, networkConsidered)
		}
		msgs = append(msgs, llm.Message{
			Role: "tool", ToolCallID: tc.ID, Name: tc.Function.Name,
			Content: respondEval(tc.Function.Name, tc.Function.Arguments, sc),
		})
	}
	return networkConsidered, fmt.Sprintf("reached %d turns without mark_done (network considered=%v)", evalMaxTurns, networkConsidered)
}

// respondEval is the synthetic tool responder for the offline drive loop.
func respondEval(tool, args string, sc evalScenario) string {
	switch tool {
	case "get_full_result", "collect", "collect_batch":
		return sc.onFullResult
	case "search_artifact":
		if mentionsNetwork(args) {
			return `{"ok":true,"data":{"artifact":"kernel.log","matches":[` +
				`{"line":18044,"text":"e1000e 0000:00:1f.6 eno1: NIC Link is Down"},` +
				`{"line":18050,"text":"bond0: link status down for interface eno1, disabling it"}]}}`
		}
		return `{"ok":true,"data":{"artifact":"kernel.log","matches":[` +
			`{"line":1,"text":"tpm tpm0: tpm_try_transmit: send command failed, err=-62"}]}}`
	case "add_finding":
		return `{"ok":true,"data":{"finding_id":"f-eval"}}`
	case "ask_operator":
		return `{"ok":true,"data":{"operator_response_pending":true}}`
	default:
		return `{"ok":true,"data":{}}`
	}
}

// kernelIndex builds the incident's kernel artifact_index: a dominant TPM storm
// plus, when withNIC is true, the rare NIC/link-flap clusters. With withNIC
// false it is the budget-collapsed headline (TPM only).
func kernelIndex(withNIC bool) logtriage.ArtifactIndex {
	clusters := []logtriage.Cluster{{
		Template: "tpm tpm0: tpm_try_transmit: send command failed, err=-62", Count: 21000,
		Severity: "warning", Unit: "kernel", FirstLine: 1, LastLine: 21000,
		Example: "tpm tpm0: tpm_try_transmit: send command failed, err=-62",
	}}
	if withNIC {
		clusters = append(clusters,
			logtriage.Cluster{
				Template: "eno1: NIC Link is Down", Count: 9, Severity: "error", Unit: "kernel",
				FirstLine: 18044, LastLine: 20933, Example: "e1000e 0000:00:1f.6 eno1: NIC Link is Down",
			},
			logtriage.Cluster{
				Template: "bond0: link status down for interface eno1, disabling it", Count: 7,
				Severity: "warning", Unit: "kernel", FirstLine: 18050, LastLine: 20940,
				Example: "bond0: link status down for interface eno1, disabling it",
			})
	}
	return logtriage.ArtifactIndex{
		Name: "kernel.log", SizeBytes: 44_000_000, LineCount: 21016,
		Units: []string{"kernel"}, TopPatterns: clusters,
	}
}

// collectResultJSON renders a collect tool result the model would see, carrying
// the given artifact_index and optional budget-collapse flag.
func collectResultJSON(idx []logtriage.ArtifactIndex, truncated bool) string {
	task := map[string]any{
		"task_id":        "t-kernel",
		"host_id":        "host-x",
		"collector":      "journal_tail",
		"status":         "done",
		"artifact_index": idx,
	}
	if truncated {
		task["_index_truncated"] = true
	}
	b, _ := json.Marshal(map[string]any{"ok": true, "data": map[string]any{"tasks": []any{task}}})
	return string(b)
}
