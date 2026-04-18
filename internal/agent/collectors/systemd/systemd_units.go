package systemd

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/vasyakrg/recon/internal/agent/collect"
	"github.com/vasyakrg/recon/internal/agent/exec"
)

func init() { collect.Register(&systemdUnits{}) }

type systemdUnits struct{}

type Unit struct {
	Unit        string `json:"unit"`
	Load        string `json:"load"`
	Active      string `json:"active"`
	Sub         string `json:"sub"`
	Description string `json:"description"`
}

type UnitsResult struct {
	Units    []Unit `json:"units"`
	Total    int    `json:"total"`
	Inactive int    `json:"inactive"`
	Failed   int    `json:"failed"`
}

func (systemdUnits) Manifest() collect.Manifest {
	return collect.Manifest{
		Name:        "systemd_units",
		Version:     "1.0.0",
		Category:    "systemd",
		Description: "Snapshot of all systemd units (load/active/sub state). Emits hints for failed and inactive critical units.",
		Reads:       []string{"systemctl list-units --all -o json"},
	}
}

func (systemdUnits) Run(ctx context.Context, _ collect.Params) (collect.Result, error) {
	res, err := exec.Run(ctx, "systemctl",
		[]string{"list-units", "--all", "--no-pager", "--no-legend", "-o", "json"})
	if err != nil {
		return collect.Result{}, fmt.Errorf("systemctl: %w", err)
	}
	out, hints := parseUnits(res.Stdout)
	return collect.Result{Data: out, Hints: hints}, nil
}

func parseUnits(body []byte) (UnitsResult, []collect.Hint) {
	out := UnitsResult{}
	if err := json.Unmarshal(body, &out.Units); err != nil {
		return out, []collect.Hint{{Severity: "error", Code: "systemd.parse_failed", Message: err.Error()}}
	}
	out.Total = len(out.Units)

	hints := []collect.Hint{}
	for _, u := range out.Units {
		switch u.Active {
		case "failed":
			out.Failed++
			hints = append(hints, collect.Hint{
				Severity: "error", Code: "service.failed",
				Message:  fmt.Sprintf("unit %s is failed (%s)", u.Unit, u.Sub),
				Evidence: map[string]any{"unit": u.Unit, "sub": u.Sub},
			})
		case "inactive":
			out.Inactive++
		}
	}
	return out, hints
}
