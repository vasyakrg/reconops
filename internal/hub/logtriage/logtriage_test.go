package logtriage

import (
	"fmt"
	"strings"
	"testing"
)

func TestTriageClusters(t *testing.T) {
	log := strings.Join([]string{
		"2026-06-19T10:00:00Z host app[123]: ERROR connection refused to 10.0.0.1:5432",
		"2026-06-19T10:00:01Z host app[124]: ERROR connection refused to 10.0.0.2:5432",
		"2026-06-19T10:00:02Z host app[125]: ERROR connection refused to 10.0.0.3:5432",
		"2026-06-19T10:00:03Z host app[126]: INFO started request 4567",
		"2026-06-19T10:00:04Z host app[127]: INFO started request 4568",
	}, "\n")
	tr := triageBytes([]byte(log))
	if tr.Lines != 5 {
		t.Fatalf("lines=%d want 5", tr.Lines)
	}
	if tr.TimeStart == "" || tr.TimeEnd == "" {
		t.Fatalf("time range missing: %q..%q", tr.TimeStart, tr.TimeEnd)
	}
	if len(tr.Clusters) != 2 {
		t.Fatalf("clusters=%d want 2: %+v", len(tr.Clusters), tr.Clusters)
	}
	top := tr.Clusters[0] // errors rank before info
	if top.Severity != "error" || top.Count != 3 {
		t.Fatalf("top cluster=%+v", top)
	}
	if top.FirstLine != 1 || top.LastLine != 3 {
		t.Fatalf("top line range %d-%d want 1-3", top.FirstLine, top.LastLine)
	}
	foundUnit := false
	for _, u := range tr.Units {
		if u == "app" {
			foundUnit = true
		}
	}
	if !foundUnit {
		t.Fatalf("unit 'app' not detected: %v", tr.Units)
	}
}

func TestIndexBinary(t *testing.T) {
	idx := IndexBytes("x.bin", []byte{0x00, 0x01, 0x02, 0x03}, 4)
	if !idx.Binary {
		t.Fatal("expected binary artifact")
	}
	if len(idx.TopPatterns) != 0 {
		t.Fatal("binary artifact should not be clustered")
	}
}

func TestIndexHeadTailAndCount(t *testing.T) {
	lines := make([]string, 0, 20)
	for i := 0; i < 20; i++ {
		lines = append(lines, fmt.Sprintf("plain line %d here", i))
	}
	idx := IndexBytes("a.log", []byte(strings.Join(lines, "\n")), 0)
	if idx.LineCount != 20 {
		t.Fatalf("line count=%d want 20", idx.LineCount)
	}
	if len(idx.FirstLines) != firstLastN || len(idx.LastLines) != firstLastN {
		t.Fatalf("head/tail = %d/%d want %d/%d", len(idx.FirstLines), len(idx.LastLines), firstLastN, firstLastN)
	}
	if !strings.Contains(idx.FirstLines[0], "line 0") {
		t.Fatalf("first line wrong: %q", idx.FirstLines[0])
	}
}

func TestRollupClustersMergesAcrossHosts(t *testing.T) {
	perHost := []HostClusters{
		{HostID: "h1", TaskID: "t1", Clusters: []Cluster{
			{Template: "oom-killer killed <n>", Severity: "critical", Count: 3, FirstLine: 10, LastLine: 40, Example: "oom h1"},
			{Template: "started request <n>", Severity: "info", Count: 5, FirstLine: 1, LastLine: 9},
		}},
		{HostID: "h2", TaskID: "t2", Clusters: []Cluster{
			{Template: "oom-killer killed <n>", Severity: "critical", Count: 7, FirstLine: 2, LastLine: 80, Example: "oom h2"},
		}},
		{HostID: "h3", TaskID: "t3", Clusters: []Cluster{
			{Template: "started request <n>", Severity: "info", Count: 2, FirstLine: 1, LastLine: 3},
		}},
	}
	rolled := RollupClusters(perHost, 8)
	if len(rolled) != 2 {
		t.Fatalf("want 2 rolled clusters, got %d: %+v", len(rolled), rolled)
	}
	// Critical ranks before info regardless of total count.
	top := rolled[0]
	if top.Severity != "critical" || top.Template != "oom-killer killed <n>" {
		t.Fatalf("top cluster should be the critical oom: %+v", top)
	}
	if top.TotalCount != 10 || top.HostCount != 2 {
		t.Fatalf("oom total=%d hostCount=%d want 10/2", top.TotalCount, top.HostCount)
	}
	// Per-host breakdown ordered by count desc — h2 (7) before h1 (3) — with
	// task_id + line refs preserved for drill-in.
	if len(top.PerHost) != 2 || top.PerHost[0].HostID != "h2" || top.PerHost[0].TaskID != "t2" {
		t.Fatalf("per-host order/refs wrong: %+v", top.PerHost)
	}
	if top.PerHost[0].FirstLine != 2 || top.PerHost[0].LastLine != 80 {
		t.Fatalf("per-host line refs not preserved: %+v", top.PerHost[0])
	}
}

func TestRollupClustersDeterministicAndDoesNotMergeAcrossSeverity(t *testing.T) {
	perHost := []HostClusters{
		{HostID: "h1", Clusters: []Cluster{{Template: "same line", Severity: "error", Count: 1}}},
		{HostID: "h2", Clusters: []Cluster{{Template: "same line", Severity: "warn", Count: 1}}},
	}
	a := RollupClusters(perHost, 8)
	b := RollupClusters(perHost, 8)
	if len(a) != 2 {
		t.Fatalf("same template, different severity must NOT merge: %+v", a)
	}
	if fmt.Sprintf("%+v", a) != fmt.Sprintf("%+v", b) {
		t.Fatalf("rollup must be deterministic:\n a=%+v\n b=%+v", a, b)
	}
	if a[0].Severity != "error" {
		t.Fatalf("error must rank before warn: %+v", a)
	}
}

func TestRollupClustersCapsHostBreakdown(t *testing.T) {
	perHost := make([]HostClusters, 0, 20)
	for i := 0; i < 20; i++ {
		perHost = append(perHost, HostClusters{
			HostID:   fmt.Sprintf("h%02d", i),
			Clusters: []Cluster{{Template: "disk full", Severity: "error", Count: i + 1}},
		})
	}
	rolled := RollupClusters(perHost, 5)
	if len(rolled) != 1 {
		t.Fatalf("want 1 cluster, got %d", len(rolled))
	}
	if len(rolled[0].PerHost) != 5 || rolled[0].OmittedHosts != 15 {
		t.Fatalf("host breakdown not capped: kept=%d omitted=%d", len(rolled[0].PerHost), rolled[0].OmittedHosts)
	}
	if rolled[0].HostCount != 20 {
		t.Fatalf("host_count should reflect all hosts: %d", rolled[0].HostCount)
	}
}

func TestNormalizeTemplateGroups(t *testing.T) {
	a := normalizeTemplate("user 42 from 192.168.1.5 opened /var/log/app/x.log")
	b := normalizeTemplate("user 99 from 10.0.0.1 opened /etc/passwd/y.log")
	if a != b {
		t.Fatalf("templates should match:\n a=%q\n b=%q", a, b)
	}
}
