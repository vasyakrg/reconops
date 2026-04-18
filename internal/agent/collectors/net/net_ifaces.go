package net

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/vasyakrg/recon/internal/agent/collect"
	"github.com/vasyakrg/recon/internal/agent/exec"
)

func init() { collect.Register(&netIfaces{}) }

type netIfaces struct{}

type IfaceInfo struct {
	Index     int        `json:"ifindex"`
	Name      string     `json:"ifname"`
	Operstate string     `json:"operstate"`
	MTU       int        `json:"mtu"`
	Address   string     `json:"address"`
	AddrInfo  []AddrInfo `json:"addr_info"`
}

type AddrInfo struct {
	Family    string `json:"family"`
	Local     string `json:"local"`
	Prefixlen int    `json:"prefixlen"`
}

type RouteInfo struct {
	Dst     string `json:"dst"`
	Gateway string `json:"gateway,omitempty"`
	Dev     string `json:"dev"`
	Proto   string `json:"protocol,omitempty"`
}

type NeighInfo struct {
	Dst    string   `json:"dst"`
	Dev    string   `json:"dev"`
	Lladdr string   `json:"lladdr,omitempty"`
	State  []string `json:"state,omitempty"`
}

type IfacesResult struct {
	Interfaces []IfaceInfo `json:"interfaces"`
	Routes     []RouteInfo `json:"routes"`
	Neighbours []NeighInfo `json:"neighbours"`
}

func (netIfaces) Manifest() collect.Manifest {
	return collect.Manifest{
		Name:        "net_ifaces",
		Version:     "1.0.0",
		Category:    "network",
		Description: "Network interfaces (link + addresses), routing table, and ARP/NDP neighbour cache. Uses `ip -json` (read-only).",
		Reads:       []string{"ip -json addr", "ip -json link", "ip -json route", "ip -json neigh"},
	}
}

func (n netIfaces) Run(ctx context.Context, _ collect.Params) (collect.Result, error) {
	addrJSON, err := runIP(ctx, "addr")
	if err != nil {
		return collect.Result{}, err
	}
	routeJSON, err := runIP(ctx, "route")
	if err != nil {
		return collect.Result{}, err
	}
	neighJSON, err := runIP(ctx, "neigh")
	if err != nil {
		return collect.Result{}, err
	}

	out := IfacesResult{}
	if err := json.Unmarshal(addrJSON, &out.Interfaces); err != nil {
		return collect.Result{}, fmt.Errorf("parse addr: %w", err)
	}
	if err := json.Unmarshal(routeJSON, &out.Routes); err != nil {
		return collect.Result{}, fmt.Errorf("parse route: %w", err)
	}
	if err := json.Unmarshal(neighJSON, &out.Neighbours); err != nil {
		return collect.Result{}, fmt.Errorf("parse neigh: %w", err)
	}

	hints := []collect.Hint{}
	for _, iface := range out.Interfaces {
		if iface.Operstate == "DOWN" && iface.Name != "lo" {
			hints = append(hints, collect.Hint{
				Severity: "warn", Code: "net.iface_down",
				Message:  fmt.Sprintf("interface %s is DOWN", iface.Name),
				Evidence: map[string]any{"iface": iface.Name},
			})
		}
	}

	return collect.Result{Data: out, Hints: hints}, nil
}

func runIP(ctx context.Context, sub string) ([]byte, error) {
	res, err := exec.Run(ctx, "ip", []string{"-json", sub})
	if err != nil {
		return nil, fmt.Errorf("ip %s: %w", sub, err)
	}
	return res.Stdout, nil
}
