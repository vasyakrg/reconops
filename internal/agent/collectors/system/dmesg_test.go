package system

import (
	"strings"
	"testing"
)

func TestBuildDmesgResult_Basic(t *testing.T) {
	stdout := []byte("2026-06-20T10:00:00,000Z kernel: tpm tpm0: timeout\n2026-06-20T10:00:01,000Z kernel: usb 1-1: ok\n")
	res, err := buildDmesgResult(stdout, 0, nil, false, "", 2000)
	if err != nil {
		t.Fatal(err)
	}
	d := res.Data.(DmesgSummary)
	if d.Lines != 2 {
		t.Fatalf("want 2 lines, got %d", d.Lines)
	}
	if string(res.Artifacts[0].Body) != string(stdout) {
		t.Fatalf("artifact body mismatch:\n%s", res.Artifacts[0].Body)
	}
}

func TestBuildDmesgResult_Grep(t *testing.T) {
	stdout := []byte("a tpm timeout\nb usb ok\nc tpm timeout again\n")
	res, err := buildDmesgResult(stdout, 0, nil, false, "tpm", 2000)
	if err != nil {
		t.Fatal(err)
	}
	d := res.Data.(DmesgSummary)
	if d.Matched != 2 || d.Lines != 2 {
		t.Fatalf("grep should keep 2 tpm lines, got matched=%d lines=%d", d.Matched, d.Lines)
	}
	if strings.Contains(string(res.Artifacts[0].Body), "usb ok") {
		t.Fatal("non-matching line leaked into artifact")
	}
}

func TestBuildDmesgResult_InvalidRegex(t *testing.T) {
	if _, err := buildDmesgResult([]byte("x\n"), 0, nil, false, "([bad", 2000); err == nil {
		t.Fatal("invalid RE2 grep must error")
	}
}

func TestBuildDmesgResult_Restricted(t *testing.T) {
	res, err := buildDmesgResult(nil, 1, []byte("dmesg: read kernel buffer failed: Operation not permitted"), false, "", 2000)
	if err != nil {
		t.Fatal(err)
	}
	d := res.Data.(DmesgSummary)
	if !d.Restricted {
		t.Fatal("expected restricted=true on dmesg_restrict block")
	}
	found := false
	for _, h := range res.Hints {
		if h.Code == "dmesg.restricted" {
			found = true
		}
	}
	if !found {
		t.Error("expected a dmesg.restricted hint")
	}
}

func TestBuildDmesgResult_MaxLines(t *testing.T) {
	var sb strings.Builder
	for i := 0; i < 10; i++ {
		sb.WriteString("line\n")
	}
	res, err := buildDmesgResult([]byte(sb.String()), 0, nil, false, "", 3)
	if err != nil {
		t.Fatal(err)
	}
	d := res.Data.(DmesgSummary)
	if d.Lines != 3 || !d.Truncated {
		t.Fatalf("want 3 lines + truncated, got lines=%d truncated=%v", d.Lines, d.Truncated)
	}
}
