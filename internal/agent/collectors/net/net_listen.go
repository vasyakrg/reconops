package net

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/vasyakrg/recon/internal/agent/collect"
	"github.com/vasyakrg/recon/internal/agent/exec"
)

func init() { collect.Register(&netListen{}) }

type netListen struct{}

type ListenSocket struct {
	Proto   string `json:"proto"`
	State   string `json:"state"`
	Local   string `json:"local"`
	Peer    string `json:"peer"`
	Process string `json:"process,omitempty"`
}

type ListenResult struct {
	Sockets []ListenSocket `json:"sockets"`
}

func (netListen) Manifest() collect.Manifest {
	return collect.Manifest{
		Name:        "net_listen",
		Version:     "1.0.0",
		Category:    "network",
		Description: "Listening TCP/UDP sockets via `ss -tulpn`. Parsed into per-socket records with binding process where available.",
		Reads:       []string{"ss -tulpn"},
		Requires:    []collect.Capability{collect.CapSudoSS},
	}
}

func (netListen) Run(ctx context.Context, _ collect.Params) (collect.Result, error) {
	res, err := exec.Run(ctx, "/usr/sbin/ss", []string{"-tulpn"})
	if err != nil {
		return collect.Result{}, fmt.Errorf("ss: %w", err)
	}
	out := ListenResult{Sockets: parseSS(string(res.Stdout))}
	return collect.Result{Data: out}, nil
}

// parseSS parses the line-oriented `ss -tulpn` output.
//
// Sample header line and rows:
//
//	Netid  State    Recv-Q  Send-Q  Local Address:Port  Peer Address:Port  Process
//	tcp    LISTEN   0       128     0.0.0.0:22          0.0.0.0:*          users:(("sshd",pid=1234,fd=3))
//	udp    UNCONN   0       0       *:68                *:*                users:(("dhclient",pid=987,fd=6))
func parseSS(body string) []ListenSocket {
	var out []ListenSocket
	procRe := regexp.MustCompile(`users:\((.*)\)`)
	for i, line := range strings.Split(body, "\n") {
		if i == 0 || strings.TrimSpace(line) == "" {
			continue
		}
		fields := strings.Fields(line)
		// netid state recv-q send-q local peer [process...]
		if len(fields) < 6 {
			continue
		}
		s := ListenSocket{
			Proto: fields[0],
			State: fields[1],
			Local: fields[4],
			Peer:  fields[5],
		}
		if len(fields) >= 7 {
			rest := strings.Join(fields[6:], " ")
			if m := procRe.FindStringSubmatch(rest); len(m) == 2 {
				s.Process = m[1]
			} else {
				s.Process = rest
			}
		}
		out = append(out, s)
	}
	return out
}
