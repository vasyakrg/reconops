// Package container holds collectors for OCI runtimes (docker today;
// crictl/podman could join later under the same exec-gateway pattern).
package container

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/vasyakrg/recon/internal/agent/collect"
	"github.com/vasyakrg/recon/internal/agent/exec"
)

// paramInt parses an optional integer param from the stringly-typed map,
// returning fallback when missing / unparseable / out of [1, max].
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

func init() {
	collect.Register(&dockerPS{})
	collect.Register(&dockerInspect{})
	collect.Register(&dockerLogs{})
}

// dockerAvailable returns true iff the docker binary resolved on this host.
// All three collectors below share it; if docker isn't installed the agent
// prunes them at startup so the LLM never tries to ps/inspect/logs nothing.
func dockerAvailable() bool { return exec.BinaryAvailable("docker") }

func (dockerPS) Available() bool      { return dockerAvailable() }
func (dockerInspect) Available() bool { return dockerAvailable() }
func (dockerLogs) Available() bool    { return dockerAvailable() }

// ── docker_ps ────────────────────────────────────────────────────────────────

type dockerPS struct{}

type dockerContainer struct {
	ID     string `json:"ID"`
	Image  string `json:"Image"`
	Names  string `json:"Names"`
	State  string `json:"State"`
	Status string `json:"Status"`
	Ports  string `json:"Ports"`
}

type dockerPSResult struct {
	Containers []dockerContainer `json:"containers"`
	Total      int               `json:"total"`
	Running    int               `json:"running"`
	Exited     int               `json:"exited"`
	Restarting int               `json:"restarting"`
}

func (dockerPS) Manifest() collect.Manifest {
	return collect.Manifest{
		Name:        "docker_ps",
		Version:     "1.0.0",
		Category:    "container",
		Description: "List all docker containers (running + stopped) with state, image, names, ports. Emits hints for restarting/exited containers.",
		Reads:       []string{"docker ps -a --no-trunc --format '{{json .}}'"},
	}
}

func (dockerPS) Run(ctx context.Context, _ collect.Params) (collect.Result, error) {
	res, err := exec.Run(ctx, "docker",
		[]string{"ps", "-a", "--no-trunc", "--format", "{{json .}}"})
	if err != nil {
		return collect.Result{}, fmt.Errorf("docker ps: %w", err)
	}
	out, hints := parsePS(res.Stdout)
	return collect.Result{Data: out, Hints: hints}, nil
}

// docker --format '{{json .}}' emits one JSON object per line, NOT a JSON
// array. Parse line-by-line and tolerate blank lines / partial reads.
func parsePS(body []byte) (dockerPSResult, []collect.Hint) {
	out := dockerPSResult{}
	hints := []collect.Hint{}
	sc := bufio.NewScanner(bytes.NewReader(body))
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var c dockerContainer
		if err := json.Unmarshal(line, &c); err != nil {
			continue
		}
		out.Containers = append(out.Containers, c)
		switch strings.ToLower(c.State) {
		case "running":
			out.Running++
		case "exited", "dead":
			out.Exited++
		case "restarting":
			out.Restarting++
			hints = append(hints, collect.Hint{
				Severity: "warn", Code: "container.restarting",
				Message:  fmt.Sprintf("container %s (%s) is restarting: %s", c.Names, c.Image, c.Status),
				Evidence: map[string]any{"container": c.Names, "id": c.ID, "image": c.Image, "status": c.Status},
			})
		}
	}
	out.Total = len(out.Containers)
	return out, hints
}

// ── docker_inspect ───────────────────────────────────────────────────────────

type dockerInspect struct{}

func (dockerInspect) Manifest() collect.Manifest {
	return collect.Manifest{
		Name:        "docker_inspect",
		Version:     "1.0.0",
		Category:    "container",
		Description: "docker inspect <container> — full state + config + mounts + network. params: {container: string}",
		Reads:       []string{"docker inspect <container>"},
	}
}

func (dockerInspect) Run(ctx context.Context, p collect.Params) (collect.Result, error) {
	c := strings.TrimSpace(p["container"])
	if c == "" {
		return collect.Result{}, fmt.Errorf("param 'container' (id or name) required")
	}
	res, err := exec.Run(ctx, "docker", []string{"inspect", c})
	if err != nil {
		return collect.Result{}, fmt.Errorf("docker inspect: %w", err)
	}
	// docker inspect always returns a JSON array — pass it through.
	var raw []json.RawMessage
	if err := json.Unmarshal(res.Stdout, &raw); err != nil {
		return collect.Result{}, fmt.Errorf("inspect parse: %w", err)
	}
	if len(raw) == 0 {
		return collect.Result{}, fmt.Errorf("container %q not found", c)
	}
	return collect.Result{Data: raw[0]}, nil
}

// ── docker_logs ──────────────────────────────────────────────────────────────

type dockerLogs struct{}

func (dockerLogs) Manifest() collect.Manifest {
	return collect.Manifest{
		Name:        "docker_logs",
		Version:     "1.0.0",
		Category:    "container",
		Description: "Tail container logs with timestamps. params: {container: string, lines?: int (default 200, max 10000)}",
		Reads:       []string{"docker logs --tail <N> --timestamps <container>"},
	}
}

func (dockerLogs) Run(ctx context.Context, p collect.Params) (collect.Result, error) {
	c := strings.TrimSpace(p["container"])
	if c == "" {
		return collect.Result{}, fmt.Errorf("param 'container' required")
	}
	lines := paramInt(p, "lines", 200, 10000)
	res, err := exec.Run(ctx, "docker",
		[]string{"logs", "--tail", fmt.Sprintf("%d", lines), "--timestamps", c})
	if err != nil {
		return collect.Result{}, fmt.Errorf("docker logs: %w", err)
	}
	return collect.Result{
		Data:      map[string]any{"container": c, "lines": lines, "bytes": len(res.Stdout)},
		Artifacts: []collect.Artifact{{Name: "docker_logs.txt", Body: res.Stdout}},
	}, nil
}
