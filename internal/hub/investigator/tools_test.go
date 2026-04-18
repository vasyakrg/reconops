package investigator

import (
	"encoding/json"
	"testing"
	"time"
)

func TestSystemPromptSubstitutes(t *testing.T) {
	out := BuildSystemPrompt("diagnose etcd", "anthropic/claude-sonnet-4.5",
		time.Date(2026, 4, 18, 10, 0, 0, 0, time.UTC), 40, 500_000)
	for _, want := range []string{
		"diagnose etcd",
		"anthropic/claude-sonnet-4.5",
		"2026-04-18T10:00:00Z",
		"40 tool calls",
		"500000 total tokens",
		"Read-only.",
	} {
		if !contains(out, want) {
			t.Errorf("system prompt missing %q", want)
		}
	}
	if contains(out, "{{") {
		t.Error("placeholder left unsubstituted")
	}
}

func TestToolsHaveValidJSONSchema(t *testing.T) {
	tools := Tools()
	if len(tools) != 11 {
		t.Fatalf("want 11 tools, got %d", len(tools))
	}
	names := map[string]bool{}
	for _, tl := range tools {
		if tl.Type != "function" {
			t.Errorf("%s: type=%s", tl.Function.Name, tl.Type)
		}
		if tl.Function.Name == "" || tl.Function.Description == "" {
			t.Errorf("missing name/desc: %+v", tl)
		}
		if names[tl.Function.Name] {
			t.Errorf("duplicate tool name: %s", tl.Function.Name)
		}
		names[tl.Function.Name] = true
		var schema map[string]any
		if err := json.Unmarshal(tl.Function.Parameters, &schema); err != nil {
			t.Errorf("%s: invalid schema JSON: %v", tl.Function.Name, err)
			continue
		}
		if schema["type"] != "object" {
			t.Errorf("%s: top-level schema must be type=object", tl.Function.Name)
		}
	}
	mustHave := []string{
		"list_hosts", "list_collectors", "describe_collector",
		"collect", "collect_batch", "search_artifact", "compare_across_hosts", "get_full_result",
		"add_finding", "ask_operator", "mark_done",
	}
	for _, n := range mustHave {
		if !names[n] {
			t.Errorf("tool %s missing from catalogue", n)
		}
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
