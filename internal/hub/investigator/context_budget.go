package investigator

import (
	"encoding/json"
	"math"

	"github.com/vasyakrg/recon/internal/hub/llm"
)

const (
	defaultContextWindowTokens = compactionTriggerTokens * 2
	defaultMaxOutputTokens     = 4096
	defaultSafetyHeadroom      = 4096

	// Calibration band for the bytes-per-token ratio (Task 6). Real
	// English/JSON sits ~3.5–4.5 bytes/token; we allow a slightly wider band
	// and never let one pathological turn push the estimate absurdly far.
	minCalibrationRatio        = 2.5
	maxCalibrationRatio        = 6.0
	calibrationEWMAAlpha       = 0.3
	minCalibrationPromptTokens = 500 // ignore tiny turns whose ratio is noise
)

type ContextBudget struct {
	EstimatedPromptTokens int
	EstimatedPromptBytes  int
	BytesPerToken         float64
	ToolSchemaTokens      int
	StaticPromptTokens    int
	ActiveMessageTokens   int
	ReservedOutputTokens  int
	SafetyHeadroomTokens  int
	ThresholdTokens       int
	AvailableInputTokens  int
	ContextWindowTokens   int
	ShouldCompact         bool
}

// NewContextBudget estimates prompt token usage and the compaction decision.
// bytesPerToken is the calibrated bytes/token ratio for this investigation
// (Task 6); <=0 falls back to the coarse compile-time default. The byte
// accounting that produced the estimate is preserved (EstimatedPromptBytes) so
// the caller can compute the next observed ratio from the provider's reported
// prompt_tokens.
func NewContextBudget(messages []llm.Message, tools []llm.Tool, contextWindowTokens, reservedOutputTokens int, bytesPerToken float64) ContextBudget {
	if contextWindowTokens <= 0 {
		contextWindowTokens = defaultContextWindowTokens
	}
	if reservedOutputTokens <= 0 {
		reservedOutputTokens = defaultMaxOutputTokens
	}
	if bytesPerToken <= 0 {
		bytesPerToken = approxBytesPerToken
	}
	toolBytes := toolSchemaBytes(tools)
	staticBytes, activeBytes := messageByteParts(messages)
	toolTokens := tokensForBytesRatio(toolBytes, bytesPerToken)
	staticTokens := tokensForBytesRatio(staticBytes, bytesPerToken)
	activeTokens := tokensForBytesRatio(activeBytes, bytesPerToken)
	estimated := toolTokens + staticTokens + activeTokens
	threshold := contextWindowTokens / 2
	available := contextWindowTokens - reservedOutputTokens - defaultSafetyHeadroom
	if available < 0 {
		available = 0
	}
	return ContextBudget{
		EstimatedPromptTokens: estimated,
		EstimatedPromptBytes:  toolBytes + staticBytes + activeBytes,
		BytesPerToken:         bytesPerToken,
		ToolSchemaTokens:      toolTokens,
		StaticPromptTokens:    staticTokens,
		ActiveMessageTokens:   activeTokens,
		ReservedOutputTokens:  reservedOutputTokens,
		SafetyHeadroomTokens:  defaultSafetyHeadroom,
		ThresholdTokens:       threshold,
		AvailableInputTokens:  available,
		ContextWindowTokens:   contextWindowTokens,
		ShouldCompact:         estimated > threshold || estimated > available,
	}
}

func messageByteParts(messages []llm.Message) (staticBytes, activeBytes int) {
	for i, m := range messages {
		b := messageBytes(m)
		if i == 0 && m.Role == "system" {
			staticBytes += b
		} else {
			activeBytes += b
		}
	}
	return staticBytes, activeBytes
}

func messageBytes(m llm.Message) int {
	bytes := len(m.Role) + len(m.Content) + len(m.Name) + len(m.ToolCallID)
	for _, tc := range m.ToolCalls {
		bytes += len(tc.ID) + len(tc.Type) + len(tc.Function.Name) + len(tc.Function.Arguments)
	}
	return bytes
}

func toolSchemaBytes(tools []llm.Tool) int {
	if len(tools) == 0 {
		return 0
	}
	body, err := json.Marshal(tools)
	if err != nil {
		return 0
	}
	return len(body)
}

func tokensForBytes(bytes int) int {
	return tokensForBytesRatio(bytes, approxBytesPerToken)
}

func tokensForBytesRatio(bytes int, bytesPerToken float64) int {
	if bytes <= 0 {
		return 0
	}
	if bytesPerToken <= 0 {
		bytesPerToken = approxBytesPerToken
	}
	return int(math.Ceil(float64(bytes) / bytesPerToken))
}

// calibrateRatio folds an observed bytes/token ratio into the prior via EWMA,
// clamped to a sane band. prev<=0 means "uncalibrated" → start from the
// observed value (still clamped). Returns the clamped result and whether the
// raw observation was outside the band (for drift logging).
func calibrateRatio(prev, observed float64) (next float64, clamped bool) {
	if observed <= 0 {
		return prev, false
	}
	next = observed
	if prev > 0 {
		next = calibrationEWMAAlpha*observed + (1-calibrationEWMAAlpha)*prev
	}
	if next < minCalibrationRatio {
		return minCalibrationRatio, true
	}
	if next > maxCalibrationRatio {
		return maxCalibrationRatio, true
	}
	return next, false
}
