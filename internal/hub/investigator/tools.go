package investigator

import (
	"encoding/json"

	"github.com/vasyakrg/recon/internal/hub/llm"
)

// Tool catalogue for the investigator. Schemas mirror BASE_TASKS.md §4 but
// are emitted in OpenAI function-calling shape so OpenRouter / vLLM / OpenAI
// all accept them as-is. Names match the shape Claude would have seen on the
// Messages API — the model is expected to behave equivalently.

// Tools returns the static catalogue. Order is significant only for display.
func Tools() []llm.Tool {
	return []llm.Tool{
		fn("list_hosts",
			"Get inventory of agents currently registered with the hub. Returns each host with: id, labels, auto-discovered facts (os, kernel, hostname, primary_ip, cpu_count, ram_gb), status (online|offline|degraded), last_seen timestamp, and the list of collectors available on that host. Use this early in an investigation to ground yourself in the fleet.",
			object(props{
				"selector": str("Optional label selector of the form 'key=value,key2=value2'. Matches hosts having ALL listed labels. Example: 'env=prod,role=k8s-master'. Omit to list every host."),
			}, nil)),
		fn("list_collectors",
			"Get the catalog of collectors implemented by agents. Each entry: { name, category, version, description, reads, requires }. Use to discover what observations are possible. Not every collector is available on every host — cross-check with host.collectors.",
			object(props{
				"category": enumStr([]string{"system", "systemd", "network", "process", "files"}, "Optional filter. Omit for all categories."),
			}, nil)),
		fn("describe_collector",
			"Get the full manifest of a collector: parameter schema with per-field descriptions, output schema, requirements (privileges, binaries), and an example output. Use when you need exact call signature before invoking.",
			object(props{
				"name": str("Collector name, e.g. 'journal_tail'."),
			}, []string{"name"})),

		fn("collect",
			"Execute ONE collector on ONE host. Returns a compact result summary, hints emitted by the collector, and references to any large artifacts. Use for targeted single-host probes.",
			object(props{
				"host_id":         str("Target host id as returned by list_hosts."),
				"collector":       str("Collector name (from list_collectors)."),
				"params":          obj("Key-value parameters per the collector's params_schema. Omit for collectors with no parameters."),
				"timeout_seconds": intRange("Max execution time. Default 30.", 1, 300),
			}, []string{"host_id", "collector"})),
		fn("collect_batch",
			"Execute the SAME collector with the SAME params on MULTIPLE hosts in parallel. Prefer this over multiple `collect` calls when surveying a fleet. Returns an array of per-host results.",
			object(props{
				"host_ids":        arrayStr("Target hosts. Min 1, max 50.", 1, 50),
				"collector":       str("Collector name."),
				"params":          obj("Per-collector parameters; same shape as collect.params."),
				"timeout_seconds": intRange("Max execution time per host.", 1, 300),
			}, []string{"host_ids", "collector"})),
		fn("search_artifact",
			"Search for a regex pattern inside a large artifact file produced by a previous collector. Returns matching lines with surrounding context. Use this INSTEAD of loading raw logs into context.",
			object(props{
				"task_id":       str("Task id that produced the artifact."),
				"artifact_name": str("Artifact filename from the task's artifact list."),
				"pattern":       str("RE2-compatible regex. Case-insensitive by default."),
				"context_lines": intRangeDefault("Lines of context per match. Default 3.", 0, 20, 3),
				"max_matches":   intRangeDefault("Cap on returned matches. Default 50.", 1, 500, 50),
			}, []string{"task_id", "artifact_name", "pattern"})),
		fn("compare_across_hosts",
			"Given task_ids that ran the SAME collector on different hosts, produce a structured diff: per-field values that agree across all hosts vs values that differ. Use to spot outliers.",
			object(props{
				"task_ids": arrayStr("Tasks to compare. Min 2, max 20.", 2, 20),
			}, []string{"task_ids"})),
		fn("get_full_result",
			"Retrieve the FULL structured output of a previous collector (not the summary). Use when the summary is insufficient and you need every field.",
			object(props{
				"task_id": str("Task id."),
			}, []string{"task_id"})),

		fn("add_finding",
			"Pin a structured diagnostic finding to the investigation memo. MUST cite at least one task_id in evidence_refs.",
			object(props{
				"severity":      enumStr([]string{"info", "warn", "error"}, "Finding severity."),
				"code":          str("Short stable code, e.g. 'etcd.cert_near_expiry'."),
				"message":       str("One-line human-readable summary."),
				"evidence_refs": arrayStr("Task ids backing this finding (minItems 1).", 1, 50),
			}, []string{"severity", "code", "message", "evidence_refs"})),
		fn("ask_operator",
			"Pause the investigation and ask the operator a question. Use for domain knowledge only the human has.",
			object(props{
				"question": str("The question to put to the operator."),
				"context":  str("Optional context summarizing what is known so far."),
			}, []string{"question"})),
		fn("mark_done",
			"Finalize the investigation with a structured post-mortem. After this call no further tool calls are allowed.",
			object(props{
				"summary": object(props{
					"symptoms":                arrayStr("Observed user-facing symptoms.", 0, 50),
					"hosts_examined":          arrayStr("host_ids touched during the investigation.", 0, 200),
					"root_cause":              str("Root-cause paragraph or 'inconclusive'."),
					"evidence_refs":           arrayStr("task_ids underpinning the conclusion.", 0, 200),
					"recommended_remediation": str("Plain-text remediation instructions for the operator."),
				}, []string{"root_cause", "recommended_remediation"}),
			}, []string{"summary"})),
	}
}

// ---- schema helpers ------------------------------------------------------

type props map[string]json.RawMessage

func fn(name, desc string, params json.RawMessage) llm.Tool {
	return llm.Tool{
		Type: "function",
		Function: llm.ToolFunction{
			Name:        name,
			Description: desc,
			Parameters:  params,
		},
	}
}

func object(p props, required []string) json.RawMessage {
	type schema struct {
		Type       string                     `json:"type"`
		Properties map[string]json.RawMessage `json:"properties"`
		Required   []string                   `json:"required,omitempty"`
		Additional bool                       `json:"additionalProperties"`
	}
	s := schema{Type: "object", Properties: map[string]json.RawMessage(p), Required: required, Additional: false}
	b, _ := json.Marshal(s)
	return b
}

func str(desc string) json.RawMessage {
	b, _ := json.Marshal(map[string]any{"type": "string", "description": desc})
	return b
}

func obj(desc string) json.RawMessage {
	b, _ := json.Marshal(map[string]any{
		"type":                 "object",
		"description":          desc,
		"additionalProperties": map[string]string{"type": "string"},
	})
	return b
}

func enumStr(values []string, desc string) json.RawMessage {
	b, _ := json.Marshal(map[string]any{"type": "string", "enum": values, "description": desc})
	return b
}

func intRange(desc string, min, max int) json.RawMessage {
	b, _ := json.Marshal(map[string]any{"type": "integer", "minimum": min, "maximum": max, "description": desc})
	return b
}

func intRangeDefault(desc string, min, max, def int) json.RawMessage {
	b, _ := json.Marshal(map[string]any{
		"type": "integer", "minimum": min, "maximum": max,
		"default": def, "description": desc,
	})
	return b
}

func arrayStr(desc string, min, max int) json.RawMessage {
	b, _ := json.Marshal(map[string]any{
		"type":        "array",
		"items":       map[string]string{"type": "string"},
		"minItems":    min,
		"maxItems":    max,
		"description": desc,
	})
	return b
}
