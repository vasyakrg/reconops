package store

import (
	"database/sql"
	"encoding/json"
	"regexp"
	"strings"
	"time"
)

const (
	TerminalKindDone           = "done"
	TerminalKindError          = "error"
	TerminalKindOperatorEnd    = "operator_end"
	TerminalKindPanic          = "panic"
	TerminalKindBudgetFinalize = "budget_finalize"
	TerminalKindLLMError       = "llm_error"
	TerminalKindInvalidHistory = "invalid_history"
)

const (
	terminalReasonLimit = 240
	terminalDetailLimit = 4096
)

var (
	terminalSecretKVPattern = regexp.MustCompile(`(?i)\b(authorization|cookie|set-cookie|api[_-]?key|access[_-]?token|refresh[_-]?token|bootstrap[_-]?token|csrf[_-]?token|password|secret)\b\s*[:=]\s*("[^"]*"|'[^']*'|Bearer\s+[A-Za-z0-9._~+/=-]{8,}|[^\s,;]+)`)
	terminalBearerPattern   = regexp.MustCompile(`(?i)\bBearer\s+[A-Za-z0-9._~+/=-]{8,}`)
	terminalTokenPattern    = regexp.MustCompile(`\b(?:sk|or|recon_pat)_[A-Za-z0-9._~+/=-]{8,}\b`)
)

// InvestigationTerminalPayload is the durable, operator-facing terminal
// summary stored in investigations.summary_json. Summary holds the legacy
// mark_done payload so existing consumers can still read final conclusions.
type InvestigationTerminalPayload struct {
	Kind        string `json:"kind"`
	Reason      string `json:"reason"`
	Detail      string `json:"detail,omitempty"`
	Recoverable bool   `json:"recoverable"`
	// Transient marks a recoverable abort that can be recovered by re-sending
	// the SAME request (transient LLM network/5xx/rate-limit), as opposed to a
	// recoverable abort that needs operator redirection (operator_end, etc.).
	// It enables a one-click "Retry last step" affordance distinct from the
	// free-text continue flow. Round-trips through Parse via the json tag.
	Transient bool            `json:"transient,omitempty"`
	Source    string          `json:"source"`
	At        time.Time       `json:"at"`
	Summary   json.RawMessage `json:"summary,omitempty"`
}

func NewInvestigationTerminalPayload(kind, reason, detail string, recoverable bool, source string, at time.Time) InvestigationTerminalPayload {
	if at.IsZero() {
		at = time.Now().UTC()
	}
	kind = strings.TrimSpace(kind)
	if kind == "" {
		kind = TerminalKindError
	}
	source = strings.TrimSpace(source)
	if source == "" {
		source = "unknown"
	}
	reason = oneLine(redactTerminalSecrets(reason), terminalReasonLimit)
	if reason == "" {
		reason = kind
	}
	return InvestigationTerminalPayload{
		Kind:        kind,
		Reason:      reason,
		Detail:      capString(strings.TrimSpace(redactTerminalSecrets(detail)), terminalDetailLimit),
		Recoverable: recoverable,
		Source:      source,
		At:          at.UTC(),
	}
}

func (p InvestigationTerminalPayload) JSON() string {
	b, err := json.Marshal(p)
	if err != nil {
		fallback := NewInvestigationTerminalPayload(TerminalKindError, "failed to encode terminal payload", err.Error(), false, "store", time.Now().UTC())
		b, _ = json.Marshal(fallback)
	}
	return string(b)
}

func TerminalDonePayload(summaryJSON string, at time.Time) InvestigationTerminalPayload {
	p := NewInvestigationTerminalPayload(TerminalKindDone, terminalDoneReason(summaryJSON), "", false, "loop", at)
	if json.Valid([]byte(summaryJSON)) {
		p.Summary = json.RawMessage(summaryJSON)
	}
	return p
}

func ParseInvestigationTerminalPayload(raw sql.NullString) (InvestigationTerminalPayload, bool) {
	if !raw.Valid || strings.TrimSpace(raw.String) == "" {
		return InvestigationTerminalPayload{}, false
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw.String), &obj); err != nil {
		return NewInvestigationTerminalPayload(TerminalKindError, "invalid terminal payload", "", true, "legacy", time.Now().UTC()), true
	}
	if _, ok := obj["kind"]; ok {
		var p InvestigationTerminalPayload
		if err := json.Unmarshal([]byte(raw.String), &p); err == nil {
			p.Kind = strings.TrimSpace(p.Kind)
			if p.Kind == "" {
				p.Kind = TerminalKindError
			}
			p.Reason = oneLine(p.Reason, terminalReasonLimit)
			if p.Reason == "" {
				p.Reason = p.Kind
			}
			p.Detail = capString(strings.TrimSpace(p.Detail), terminalDetailLimit)
			if p.At.IsZero() {
				p.At = time.Now().UTC()
			}
			return p, true
		}
	}
	return parseLegacyTerminalPayload(obj, raw.String), true
}

func parseLegacyTerminalPayload(obj map[string]json.RawMessage, raw string) InvestigationTerminalPayload {
	for _, key := range []string{"error", "reason", "summary"} {
		if v, ok := obj[key]; ok {
			var text string
			if err := json.Unmarshal(v, &text); err == nil {
				kind := TerminalKindError
				source := "legacy"
				recoverable := true
				if key == "summary" && strings.EqualFold(text, "operator ended") {
					kind = TerminalKindOperatorEnd
					source = "operator"
				}
				return NewInvestigationTerminalPayload(kind, text, text, recoverable, source, time.Now().UTC())
			}
		}
	}
	return NewInvestigationTerminalPayload(TerminalKindDone, terminalDoneReason(raw), "", false, "legacy", time.Now().UTC())
}

func terminalDoneReason(summaryJSON string) string {
	var obj struct {
		RootCause  string `json:"root_cause"`
		Confidence string `json:"confidence"`
	}
	if err := json.Unmarshal([]byte(summaryJSON), &obj); err == nil && strings.TrimSpace(obj.RootCause) != "" {
		if c := strings.TrimSpace(obj.Confidence); c != "" {
			// Surface honest confidence in the operator-facing reason and in the
			// "Prior conclusion" carried into a reopen, so a speculative/inconclusive
			// close is never mistaken for a confirmed one.
			return "Investigation complete [" + c + "]: " + obj.RootCause
		}
		return "Investigation complete: " + obj.RootCause
	}
	return "Investigation complete"
}

func oneLine(s string, limit int) string {
	s = strings.TrimSpace(strings.ReplaceAll(s, "\n", " "))
	return capString(strings.Join(strings.Fields(s), " "), limit)
}

func capString(s string, limit int) string {
	if limit <= 0 || len(s) <= limit {
		return s
	}
	if limit <= 3 {
		return s[:limit]
	}
	return s[:limit-3] + "..."
}

func redactTerminalSecrets(s string) string {
	if s == "" {
		return ""
	}
	s = terminalSecretKVPattern.ReplaceAllString(s, `$1=[REDACTED]`)
	s = terminalBearerPattern.ReplaceAllString(s, `Bearer [REDACTED]`)
	return terminalTokenPattern.ReplaceAllString(s, `[REDACTED_TOKEN]`)
}
