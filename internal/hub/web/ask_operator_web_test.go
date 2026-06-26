package web

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/vasyakrg/recon/internal/hub/store"
)

// A pending ask_operator must render the dedicated answer card (question +
// answer box posting to /investigations/answer), not the raw JSON editor.
func TestAskOperatorPendingRendersAnswerField(t *testing.T) {
	srv, st := newTestServer(t)
	ctx := context.Background()
	if err := st.InsertInvestigation(ctx, store.Investigation{
		ID: "inv-ask", Goal: "g", Status: "active", CreatedBy: "o", Model: "m", BaseURL: "u",
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.InsertToolCall(ctx, store.ToolCallRow{
		ID: "q1", InvestigationID: "inv-ask", Seq: 1, Tool: "ask_operator",
		InputJSON: `{"question":"which node runs etcd?"}`, Status: "pending",
	}); err != nil {
		t.Fatal(err)
	}
	sid, _, err := srv.sessions.issue(ctx, "admin", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	body := fetchFragment(t, srv, sid, "inv-ask")
	if !strings.Contains(body, `action="/investigations/answer"`) || !strings.Contains(body, `name="answer"`) {
		t.Fatalf("ask_operator pending must render a dedicated answer form")
	}
	if !strings.Contains(body, "Model is asking") || !strings.Contains(body, "which node runs etcd?") {
		t.Fatalf("answer card must surface the model's question")
	}
}
