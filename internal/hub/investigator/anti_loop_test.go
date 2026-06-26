package investigator

import (
	"context"
	"testing"

	"github.com/vasyakrg/recon/internal/hub/store"
)

func insertExecutedTC(t *testing.T, st *store.Store, inv, id, tool, input string, seq int) {
	t.Helper()
	ctx := context.Background()
	if err := st.InsertToolCall(ctx, store.ToolCallRow{ID: id, InvestigationID: inv, Seq: seq, Tool: tool, InputJSON: input, Status: "approved"}); err != nil {
		t.Fatal(err)
	}
	// A successfully executed tool call records ok:true (the realistic state);
	// askOperatorStreak now relies on this to exclude preflight-blocked re-asks.
	if err := st.UpdateToolCall(ctx, id, "executed", "auto", "task-"+id, `{"ok":true}`); err != nil {
		t.Fatal(err)
	}
}

func TestRedundantFileReadGuard(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	const inv = "inv-rr"
	if err := st.InsertInvestigation(ctx, store.Investigation{ID: inv, Goal: "g", Status: "active", CreatedBy: "o", Model: "m", BaseURL: "u"}); err != nil {
		t.Fatal(err)
	}
	insertExecutedTC(t, st, inv, "c1", "collect",
		`{"collector":"file_read","host_id":"h1","params":{"path":"/var/log/syslog","max_bytes":"200000"}}`, 1)
	l := &Loop{store: st}

	// Same path + same (default head) region, only max_bytes differs → blocked.
	bigger := &store.ToolCallRow{ID: "c2", InvestigationID: inv, Tool: "collect",
		InputJSON: `{"collector":"file_read","host_id":"h1","params":{"path":"/var/log/syslog","max_bytes":"1048576"}}`}
	if _, blocked := l.preflightCollectEconomy(ctx, inv, bigger); !blocked {
		t.Fatal("re-reading the same region with a larger max_bytes must be blocked")
	}

	// Head→tail escalation (from_end) is a DIFFERENT region → must NOT be blocked.
	tail := &store.ToolCallRow{ID: "c3", InvestigationID: inv, Tool: "collect",
		InputJSON: `{"collector":"file_read","host_id":"h1","params":{"path":"/var/log/syslog","from_end":"true","max_bytes":"1048576"}}`}
	if _, blocked := l.preflightCollectEconomy(ctx, inv, tail); blocked {
		t.Fatal("a head→tail (from_end) read on the same path must NOT be flagged redundant")
	}
}

func TestAskOperatorStreak(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	const inv = "inv-streak"
	if err := st.InsertInvestigation(ctx, store.Investigation{ID: inv, Goal: "g", Status: "active", CreatedBy: "o", Model: "m", BaseURL: "u"}); err != nil {
		t.Fatal(err)
	}
	for i, id := range []string{"a1", "a2", "a3"} {
		insertExecutedTC(t, st, inv, id, "ask_operator", `{"question":"incident time?"}`, i+1)
	}
	l := &Loop{store: st}
	if n := l.askOperatorStreak(ctx, inv); n != 3 {
		t.Fatalf("want streak 3, got %d", n)
	}
	// An intervening executed collect resets the streak.
	insertExecutedTC(t, st, inv, "c1", "collect", `{"collector":"system_info","host_id":"h1"}`, 4)
	if n := l.askOperatorStreak(ctx, inv); n != 0 {
		t.Fatalf("an executed collect must reset the streak, got %d", n)
	}
}
