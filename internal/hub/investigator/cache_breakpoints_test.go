package investigator

import (
	"testing"

	"github.com/vasyakrg/recon/internal/hub/llm"
)

func TestMarkCacheBreakpointsDisabledIsNoop(t *testing.T) {
	msgs := []llm.Message{
		{Role: "system", Content: "prompt"},
		{Role: "user", Content: "goal"},
	}
	if n := markCacheBreakpoints(msgs, false); n != 0 {
		t.Fatalf("disabled route must set 0 breakpoints, got %d", n)
	}
	for _, m := range msgs {
		if m.CacheControl {
			t.Fatalf("no message may be marked when cache is unsupported: %+v", m)
		}
	}
}

func TestMarkCacheBreakpointsMarksSystemPrefixAndSummary(t *testing.T) {
	msgs := []llm.Message{
		{Role: "system", Content: "stable system prompt"},
		{Role: "user", Content: "goal"},
		{Role: "assistant", Content: "thinking"},
		{Role: "system", Content: "COMPACT_STATE memory_id=m1 ..."}, // compaction summary
		{Role: "tool", Content: `{"ok":true}`, ToolCallID: "c1"},
	}
	n := markCacheBreakpoints(msgs, true)
	if n != 2 {
		t.Fatalf("want 2 breakpoints (system prefix + summary), got %d", n)
	}
	if !msgs[0].CacheControl {
		t.Fatalf("system prefix at index 0 must be a breakpoint")
	}
	if !msgs[3].CacheControl {
		t.Fatalf("the last system message (compaction summary) must be a breakpoint")
	}
	if msgs[1].CacheControl || msgs[2].CacheControl || msgs[4].CacheControl {
		t.Fatalf("only system messages may be breakpoints: %+v", msgs)
	}
}

func TestMarkCacheBreakpointsSinglePrefixOnly(t *testing.T) {
	msgs := []llm.Message{
		{Role: "system", Content: "stable system prompt"},
		{Role: "user", Content: "goal"},
		{Role: "tool", Content: `{"ok":true}`, ToolCallID: "c1"},
	}
	if n := markCacheBreakpoints(msgs, true); n != 1 {
		t.Fatalf("want exactly 1 breakpoint on the system prefix, got %d", n)
	}
	if !msgs[0].CacheControl {
		t.Fatalf("system prefix must be marked")
	}
}
