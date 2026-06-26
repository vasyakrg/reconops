package store

import (
	"database/sql"
	"testing"
	"time"
)

func TestTerminalPayloadTransientRoundTrips(t *testing.T) {
	p := NewInvestigationTerminalPayload(TerminalKindLLMError,
		"llm network error", "connect: connection refused", true, "llm", time.Now().UTC())
	p.Transient = true

	parsed, ok := ParseInvestigationTerminalPayload(sql.NullString{String: p.JSON(), Valid: true})
	if !ok {
		t.Fatal("expected payload to parse")
	}
	if !parsed.Transient {
		t.Fatalf("Transient must round-trip through JSON/parse: %+v", parsed)
	}
	if parsed.Kind != TerminalKindLLMError || parsed.Source != "llm" || !parsed.Recoverable {
		t.Fatalf("other fields must survive: %+v", parsed)
	}

	// A non-transient recoverable abort must not be flagged.
	q := NewInvestigationTerminalPayload(TerminalKindOperatorEnd, "operator ended", "", true, "operator", time.Now().UTC())
	parsedQ, _ := ParseInvestigationTerminalPayload(sql.NullString{String: q.JSON(), Valid: true})
	if parsedQ.Transient {
		t.Fatalf("operator_end must not be transient: %+v", parsedQ)
	}
}
