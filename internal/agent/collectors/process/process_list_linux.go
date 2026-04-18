//go:build linux

package process

import (
	"bufio"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// readProcesses scans /proc for numeric directories and parses /proc/{pid}/stat
// + status. Errors on individual processes are ignored — they may have exited
// between the readdir and the open.
func readProcesses(procRoot string) ([]Process, error) {
	entries, err := os.ReadDir(procRoot)
	if err != nil {
		return nil, err
	}
	var out []Process
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		p, err := readOne(filepath.Join(procRoot, e.Name()), pid)
		if err != nil {
			continue
		}
		out = append(out, p)
	}
	return out, nil
}

func readOne(dir string, pid int) (Process, error) {
	p := Process{PID: pid}

	statBody, err := os.ReadFile(filepath.Join(dir, "stat"))
	if err != nil {
		return p, err
	}
	parseStat(statBody, &p)

	statusBody, err := os.ReadFile(filepath.Join(dir, "status"))
	if err == nil {
		parseStatus(statusBody, &p)
	}
	return p, nil
}

// parseStat reads /proc/{pid}/stat. The comm field is in parens and may
// contain spaces; we slice from the last ')' to handle that.
func parseStat(body []byte, p *Process) {
	s := string(body)
	end := strings.LastIndex(s, ")")
	if end < 0 {
		return
	}
	start := strings.IndexByte(s, '(')
	if start < 0 || start >= end {
		return
	}
	p.Comm = s[start+1 : end]

	rest := strings.Fields(strings.TrimSpace(s[end+1:]))
	// Field indices per proc(5), counting from rest[0] = state (field 3 in
	// the manpage).
	if len(rest) >= 1 {
		p.State = rest[0]
	}
	if len(rest) >= 2 {
		if v, err := strconv.Atoi(rest[1]); err == nil {
			p.PPID = v
		}
	}
	// utime (12) + stime (13) — indices 11 and 12 in rest.
	if len(rest) >= 13 {
		ut, _ := strconv.ParseUint(rest[11], 10, 64)
		st, _ := strconv.ParseUint(rest[12], 10, 64)
		p.CPUS = ut + st
	}
}

func parseStatus(body []byte, p *Process) {
	sc := bufio.NewScanner(strings.NewReader(string(body)))
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "VmRSS:"):
			p.RSSKB = parseKB(line)
		case strings.HasPrefix(line, "VmSize:"):
			p.VMSize = parseKB(line)
		case strings.HasPrefix(line, "Uid:"):
			fields := strings.Fields(line)
			if len(fields) >= 2 {
				if v, err := strconv.Atoi(fields[1]); err == nil {
					p.UID = v
				}
			}
		}
	}
}

func parseKB(line string) uint64 {
	fields := strings.Fields(line)
	if len(fields) < 2 {
		return 0
	}
	v, _ := strconv.ParseUint(fields[1], 10, 64)
	return v
}
