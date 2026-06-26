package investigator

import (
	"encoding/json"
	"testing"

	"github.com/vasyakrg/recon/internal/hub/llm"
)

func TestContextBudgetCompactsAtHalfContextWindow(t *testing.T) {
	msgs := []llm.Message{
		{Role: "system", Content: "static"},
		{Role: "user", Content: string(make([]byte, 240))},
	}
	budget := NewContextBudget(msgs, nil, 100, 20, 0)
	if budget.ThresholdTokens != 50 {
		t.Fatalf("threshold=%d", budget.ThresholdTokens)
	}
	if !budget.ShouldCompact {
		t.Fatalf("expected compaction when estimate crosses half window: %+v", budget)
	}
	if budget.ReservedOutputTokens != 20 || budget.SafetyHeadroomTokens != defaultSafetyHeadroom {
		t.Fatalf("reserve/headroom wrong: %+v", budget)
	}
}

func TestContextBudgetIncludesToolSchemaTokens(t *testing.T) {
	tool := llm.Tool{Type: "function", Function: llm.ToolFunction{
		Name:       "list_hosts",
		Parameters: json.RawMessage(`{"type":"object","properties":{"selector":{"type":"string"}}}`),
	}}
	withoutTools := NewContextBudget([]llm.Message{{Role: "system", Content: "static"}}, nil, 10000, 100, 0)
	withTools := NewContextBudget([]llm.Message{{Role: "system", Content: "static"}}, []llm.Tool{tool}, 10000, 100, 0)
	if withTools.ToolSchemaTokens <= 0 {
		t.Fatalf("expected tool schema tokens: %+v", withTools)
	}
	if withTools.EstimatedPromptTokens <= withoutTools.EstimatedPromptTokens {
		t.Fatalf("tool schemas should increase prompt estimate: without=%+v with=%+v", withoutTools, withTools)
	}
}

func TestContextBudgetCalibratedRatioChangesEstimate(t *testing.T) {
	msgs := []llm.Message{
		{Role: "system", Content: "static"},
		{Role: "user", Content: string(make([]byte, 4000))},
	}
	// A smaller bytes/token ratio means more tokens per byte → higher estimate.
	def := NewContextBudget(msgs, nil, 100000, 4096, 0)      // default ~4
	dense := NewContextBudget(msgs, nil, 100000, 4096, 2.5)  // log-dense JSON
	sparse := NewContextBudget(msgs, nil, 100000, 4096, 6.0) // prose
	if dense.EstimatedPromptTokens <= def.EstimatedPromptTokens {
		t.Fatalf("smaller ratio must raise the estimate: def=%d dense=%d", def.EstimatedPromptTokens, dense.EstimatedPromptTokens)
	}
	if sparse.EstimatedPromptTokens >= def.EstimatedPromptTokens {
		t.Fatalf("larger ratio must lower the estimate: def=%d sparse=%d", def.EstimatedPromptTokens, sparse.EstimatedPromptTokens)
	}
	if def.EstimatedPromptBytes <= 0 {
		t.Fatalf("budget must expose the byte count used for calibration: %+v", def)
	}
}

func TestCalibrateRatioEWMAAndClamp(t *testing.T) {
	// Uncalibrated start adopts the (clamped) observation.
	if got, _ := calibrateRatio(0, 4.2); got != 4.2 {
		t.Fatalf("uncalibrated should adopt observed: got %v", got)
	}
	// EWMA moves the prior toward the observation but not all the way.
	got, clamped := calibrateRatio(4.0, 3.0)
	if clamped {
		t.Fatalf("3.0 is within band, should not clamp")
	}
	if got >= 4.0 || got <= 3.0 {
		t.Fatalf("EWMA should land strictly between prior and observation: %v", got)
	}
	// Out-of-band observations clamp.
	if got, clamped := calibrateRatio(0, 0.5); !clamped || got != minCalibrationRatio {
		t.Fatalf("tiny ratio must clamp to min: got=%v clamped=%v", got, clamped)
	}
	if got, clamped := calibrateRatio(0, 50); !clamped || got != maxCalibrationRatio {
		t.Fatalf("huge ratio must clamp to max: got=%v clamped=%v", got, clamped)
	}
}
