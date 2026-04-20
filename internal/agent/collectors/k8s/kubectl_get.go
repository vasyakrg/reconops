// Package k8s holds collectors that talk to kubectl on the host. Read-only:
// only `kubectl get -o json`, `describe`, and `logs --tail` reach the
// exec gateway. The agent uses whatever kubeconfig is available on the
// host (typically /root/.kube/config or /etc/kubernetes/admin.conf) — no
// cluster credentials are stored on the agent itself.
package k8s

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/vasyakrg/recon/internal/agent/collect"
	"github.com/vasyakrg/recon/internal/agent/exec"
)

func init() {
	collect.Register(&kubectlGet{})
	collect.Register(&kubectlDescribe{})
	collect.Register(&kubectlLogs{})
}

// kubectlAvailable gates all three kubectl_* collectors. On a host without
// kubectl, the LLM never sees them as candidates — saves a round-trip and
// a confusing "no such file" error message.
func kubectlAvailable() bool { return exec.BinaryAvailable("kubectl") }

func (kubectlGet) Available() bool      { return kubectlAvailable() }
func (kubectlDescribe) Available() bool { return kubectlAvailable() }
func (kubectlLogs) Available() bool     { return kubectlAvailable() }

// paramInt parses an optional integer param from the stringly-typed map.
func paramInt(p collect.Params, key string, fallback, max int) int {
	v, ok := p[key]
	if !ok || v == "" {
		return fallback
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 || n > max {
		return fallback
	}
	return n
}

// ── kubectl_get ──────────────────────────────────────────────────────────────

type kubectlGet struct{}

type k8sListResult struct {
	Resource     string         `json:"resource"`
	Namespace    string         `json:"namespace,omitempty"`
	Total        int            `json:"total"`
	NotReady     int            `json:"not_ready,omitempty"`
	Items        []k8sItem      `json:"items"`
	APIServerErr string         `json:"apiserver_error,omitempty"`
	Raw          map[string]any `json:"-"`
}

type k8sItem struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace,omitempty"`
	Kind      string `json:"kind,omitempty"`
	Status    string `json:"status,omitempty"`
	Ready     string `json:"ready,omitempty"`
	Restarts  int    `json:"restarts,omitempty"`
	Age       string `json:"age,omitempty"`
}

func (kubectlGet) Manifest() collect.Manifest {
	return collect.Manifest{
		Name:        "kubectl_get",
		Version:     "1.0.0",
		Category:    "k8s",
		Description: "kubectl get <resource> -o json — list any resource (pods, nodes, deployments, services, ...). params: {resource: string (required), namespace: string ('' = all namespaces)}",
		Reads:       []string{"kubectl get <resource> [-A | -n <ns>] -o json"},
	}
}

func (kubectlGet) Run(ctx context.Context, p collect.Params) (collect.Result, error) {
	resource := strings.TrimSpace(p["resource"])
	if resource == "" {
		return collect.Result{}, fmt.Errorf("param 'resource' required (e.g. pods, nodes, deployments)")
	}
	ns := strings.TrimSpace(p["namespace"])
	args := []string{"get", resource}
	if ns == "" {
		args = append(args, "-A")
	} else {
		args = append(args, "-n", ns)
	}
	args = append(args, "-o", "json")
	res, err := exec.Run(ctx, "kubectl", args)
	if err != nil {
		return collect.Result{}, fmt.Errorf("kubectl get %s: %w", resource, err)
	}
	out, hints := parseList(resource, ns, res.Stdout)
	return collect.Result{
		Data:      out,
		Hints:     hints,
		Artifacts: []collect.Artifact{{Name: "kubectl_get_" + safeName(resource) + ".json", Body: res.Stdout}},
	}, nil
}

func parseList(resource, ns string, body []byte) (k8sListResult, []collect.Hint) {
	out := k8sListResult{Resource: resource, Namespace: ns}
	hints := []collect.Hint{}
	var raw struct {
		Kind  string `json:"kind"`
		Items []struct {
			Metadata struct {
				Name      string `json:"name"`
				Namespace string `json:"namespace"`
			} `json:"metadata"`
			Kind   string `json:"kind"`
			Status struct {
				Phase             string `json:"phase"`
				ContainerStatuses []struct {
					Ready        bool `json:"ready"`
					RestartCount int  `json:"restartCount"`
				} `json:"containerStatuses"`
				Conditions []struct {
					Type   string `json:"type"`
					Status string `json:"status"`
				} `json:"conditions"`
			} `json:"status"`
		} `json:"items"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		out.APIServerErr = err.Error()
		return out, hints
	}
	out.Total = len(raw.Items)
	for _, it := range raw.Items {
		row := k8sItem{
			Name:      it.Metadata.Name,
			Namespace: it.Metadata.Namespace,
			Kind:      it.Kind,
			Status:    it.Status.Phase,
		}
		// Ready ratio + total restarts (Pod-shaped). Best-effort, no-op for
		// other resource kinds where ContainerStatuses is empty.
		ready, total := 0, len(it.Status.ContainerStatuses)
		for _, cs := range it.Status.ContainerStatuses {
			if cs.Ready {
				ready++
			}
			row.Restarts += cs.RestartCount
		}
		if total > 0 {
			row.Ready = fmt.Sprintf("%d/%d", ready, total)
			if ready < total {
				out.NotReady++
				hints = append(hints, collect.Hint{
					Severity: "warn", Code: "pod.not_ready",
					Message:  fmt.Sprintf("%s/%s ready %d/%d (status=%s, restarts=%d)", it.Metadata.Namespace, it.Metadata.Name, ready, total, it.Status.Phase, row.Restarts),
					Evidence: map[string]any{"namespace": it.Metadata.Namespace, "name": it.Metadata.Name, "ready": row.Ready, "restarts": row.Restarts, "status": it.Status.Phase},
				})
			}
		}
		// Node-shaped: surface NotReady condition.
		for _, c := range it.Status.Conditions {
			if c.Type == "Ready" && c.Status != "True" {
				out.NotReady++
				hints = append(hints, collect.Hint{
					Severity: "warn", Code: "node.not_ready",
					Message:  fmt.Sprintf("node %s Ready=%s", it.Metadata.Name, c.Status),
					Evidence: map[string]any{"node": it.Metadata.Name, "ready": c.Status},
				})
			}
		}
		out.Items = append(out.Items, row)
	}
	return out, hints
}

// ── kubectl_describe ─────────────────────────────────────────────────────────

type kubectlDescribe struct{}

func (kubectlDescribe) Manifest() collect.Manifest {
	return collect.Manifest{
		Name:        "kubectl_describe",
		Version:     "1.0.0",
		Category:    "k8s",
		Description: "kubectl describe <resource> <name> -n <ns> — full event/condition log for one object. params: {resource, name, namespace} (all required).",
		Reads:       []string{"kubectl describe <resource> <name> -n <ns>"},
	}
}

func (kubectlDescribe) Run(ctx context.Context, p collect.Params) (collect.Result, error) {
	resource := strings.TrimSpace(p["resource"])
	name := strings.TrimSpace(p["name"])
	ns := strings.TrimSpace(p["namespace"])
	if resource == "" || name == "" || ns == "" {
		return collect.Result{}, fmt.Errorf("params 'resource', 'name', 'namespace' all required")
	}
	res, err := exec.Run(ctx, "kubectl", []string{"describe", resource, name, "-n", ns})
	if err != nil {
		return collect.Result{}, fmt.Errorf("kubectl describe: %w", err)
	}
	return collect.Result{
		Data:      map[string]any{"resource": resource, "name": name, "namespace": ns, "bytes": len(res.Stdout)},
		Artifacts: []collect.Artifact{{Name: "kubectl_describe.txt", Body: res.Stdout}},
	}, nil
}

// ── kubectl_logs ─────────────────────────────────────────────────────────────

type kubectlLogs struct{}

func (kubectlLogs) Manifest() collect.Manifest {
	return collect.Manifest{
		Name:        "kubectl_logs",
		Version:     "1.0.0",
		Category:    "k8s",
		Description: "kubectl logs <pod> -n <ns> --tail <N> --timestamps. params: {pod, namespace, lines? (default 200, max 10000)}.",
		Reads:       []string{"kubectl logs <pod> -n <ns> --tail <N> --timestamps"},
	}
}

func (kubectlLogs) Run(ctx context.Context, p collect.Params) (collect.Result, error) {
	pod := strings.TrimSpace(p["pod"])
	ns := strings.TrimSpace(p["namespace"])
	if pod == "" || ns == "" {
		return collect.Result{}, fmt.Errorf("params 'pod' and 'namespace' required")
	}
	lines := paramInt(p, "lines", 200, 10000)
	res, err := exec.Run(ctx, "kubectl",
		[]string{"logs", pod, "-n", ns, "--tail", fmt.Sprintf("%d", lines), "--timestamps"})
	if err != nil {
		return collect.Result{}, fmt.Errorf("kubectl logs: %w", err)
	}
	return collect.Result{
		Data:      map[string]any{"pod": pod, "namespace": ns, "lines": lines, "bytes": len(res.Stdout)},
		Artifacts: []collect.Artifact{{Name: "kubectl_logs.txt", Body: res.Stdout}},
	}, nil
}

// safeName strips characters that don't belong in a filename. The artifact
// name is operator-facing only; collisions are fine (overwritten per call).
func safeName(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			return r
		}
		return '_'
	}, s)
}
