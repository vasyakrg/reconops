package investigator

import (
	"strings"
	"testing"

	"github.com/vasyakrg/recon/internal/hub/logtriage"
)

// makeHeavyTask builds a task view whose artifact_index carries enough cluster
// detail + line samples to blow a small token budget on its own.
func makeHeavyTask(taskID, hostID string) taskView {
	clusters := make([]logtriage.Cluster, 0, 12)
	for i := 0; i < 12; i++ {
		clusters = append(clusters, logtriage.Cluster{
			Template: "kernel: <hex> page allocation failure order <n> node <n> " + strings.Repeat("x", 60),
			Count:    100 - i,
			Severity: "error",
			Example:  strings.Repeat("ERROR ", 60),
		})
	}
	lines := make([]string, 0, 5)
	for i := 0; i < 5; i++ {
		lines = append(lines, strings.Repeat("sample log line ", 20))
	}
	return taskView{
		TaskID: taskID, HostID: hostID, Collector: "journal", Status: "done",
		ArtifactIndex: []logtriage.ArtifactIndex{{
			Name:        "journal.log",
			SizeBytes:   12 << 20,
			LineCount:   500000,
			TopPatterns: clusters,
			FirstLines:  lines,
			LastLines:   lines,
		}},
	}
}

func TestApplyResultBudgetUnderBudgetUntouched(t *testing.T) {
	views := []taskView{{TaskID: "t1", HostID: "h1", Collector: "system_info", Status: "done"}}
	got, meta, outcome := applyResultBudget(views, 2000)
	if outcome.Demoted {
		t.Fatalf("small result should not be demoted: %+v", outcome)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 task kept, got %d", len(got))
	}
	if _, ok := meta["_hint"]; ok {
		t.Fatalf("untouched result should not carry _hint: %+v", meta)
	}
	if got[0].IndexTruncated {
		t.Fatalf("untouched task should not be flagged _index_truncated")
	}
}

func TestApplyResultBudgetCollapsesHeavyIndex(t *testing.T) {
	views := []taskView{makeHeavyTask("t1", "h1")}
	got, meta, outcome := applyResultBudget(views, 300)
	if !outcome.Demoted {
		t.Fatalf("heavy result should be demoted under a 300-token cap")
	}
	if outcome.FinalTokens > outcome.MaxTokens {
		t.Fatalf("final tokens %d should be within cap %d", outcome.FinalTokens, outcome.MaxTokens)
	}
	if outcome.FinalTokens >= outcome.EstimatedTokens {
		t.Fatalf("demotion should reduce tokens: before=%d after=%d", outcome.EstimatedTokens, outcome.FinalTokens)
	}
	if meta["_hint"] == nil {
		t.Fatalf("demoted result must steer the model with _hint")
	}
	if !got[0].IndexTruncated {
		t.Fatalf("collapsed task must be flagged _index_truncated: %+v", got[0])
	}
	// Headline keeps navigation metadata + exactly one cluster.
	idx := got[0].ArtifactIndex[0]
	if idx.Name != "journal.log" || idx.SizeBytes == 0 || idx.LineCount == 0 {
		t.Fatalf("headline must retain name/size/line_count: %+v", idx)
	}
	if len(idx.TopPatterns) != 1 {
		t.Fatalf("headline must keep exactly the top-1 cluster, got %d", len(idx.TopPatterns))
	}
	if idx.FirstLines != nil || idx.LastLines != nil {
		t.Fatalf("headline must drop line samples: %+v", idx)
	}
}

func TestApplyResultBudgetDropsTasksAsLastResort(t *testing.T) {
	views := make([]taskView, 0, 40)
	for i := 0; i < 40; i++ {
		views = append(views, makeHeavyTask("t"+string(rune('a'+i%26))+string(rune('0'+i/26)), "h"+string(rune('a'+i%26))))
	}
	got, meta, outcome := applyResultBudget(views, 200)
	if !outcome.Demoted {
		t.Fatalf("40 heavy tasks must be demoted under a 200-token cap")
	}
	if outcome.OmittedTasks == 0 {
		t.Fatalf("expected some tasks omitted as last resort, steps=%v", outcome.DemotionSteps)
	}
	if len(got) < 1 {
		t.Fatalf("at least one task must remain as a navigable anchor")
	}
	if meta["_omitted_tasks"] == nil {
		t.Fatalf("dropped tasks must be reported via _omitted_tasks: %+v", meta)
	}
	if got[0].TaskID == "" {
		t.Fatalf("retained anchor must keep its task_id for get_full_result")
	}
}

// makeLogTask builds a task view carrying a clustered log artifact_index for a
// given collector, so the roll-up path has something to merge.
func makeLogTask(taskID, hostID, collector, template, severity string, count int) taskView {
	return taskView{
		TaskID: taskID, HostID: hostID, Collector: collector, Status: "done",
		Artifacts: []string{"journal.log"},
		ArtifactIndex: []logtriage.ArtifactIndex{{
			Name:      "journal.log",
			SizeBytes: 4 << 20,
			LineCount: 9000,
			TopPatterns: []logtriage.Cluster{{
				Template: template, Severity: severity, Count: count,
				FirstLine: 1, LastLine: count, Example: "EXAMPLE " + hostID,
			}},
		}},
	}
}

func TestMaybeBatchRollupAppliesAcrossHosts(t *testing.T) {
	views := []taskView{
		makeLogTask("t1", "h1", "journal", "oom-killer killed <n>", "critical", 5),
		makeLogTask("t2", "h2", "journal", "oom-killer killed <n>", "critical", 9),
		makeLogTask("t3", "h3", "journal", "tcp retransmit to <ip>", "warn", 3),
	}
	rollup, stats, applied, differ := maybeBatchRollup(views, rollupMaxHostsPerCluster)
	if !applied || differ {
		t.Fatalf("rollup should apply across same-collector hosts: applied=%v differ=%v", applied, differ)
	}
	if rollup.Collector != "journal" || rollup.HostCount != 3 {
		t.Fatalf("rollup metadata wrong: %+v", rollup)
	}
	if stats.HostsCovered != 3 || stats.ClustersBefore != 3 {
		t.Fatalf("rollup stats wrong: %+v", stats)
	}

	res := summarizeWithRollup(HandlerEnv{InvestigationID: "inv1"}, views, rollup, stats, 2000)
	if !res.OK {
		t.Fatalf("rollup result should be ok: %+v", res)
	}
	data, ok := res.Data.(map[string]any)
	if !ok {
		t.Fatalf("result data should be a map, got %T", res.Data)
	}
	br, ok := data["batch_rollup"].(*batchRollup)
	if !ok {
		t.Fatalf("result must carry batch_rollup, got %T", data["batch_rollup"])
	}
	if len(br.Clusters) == 0 || br.Clusters[0].Severity != "critical" {
		t.Fatalf("rollup clusters should lead with the critical oom: %+v", br.Clusters)
	}
	// Per-task entries become drill-in headlines: task_id + artifact name kept,
	// repeated TopPatterns dropped (they now live in the roll-up).
	tasks, ok := data["tasks"].([]taskView)
	if !ok {
		t.Fatalf("result must carry tasks slice, got %T", data["tasks"])
	}
	for _, tv := range tasks {
		if tv.TaskID == "" {
			t.Fatalf("drill-in headline must keep task_id: %+v", tv)
		}
		for _, idx := range tv.ArtifactIndex {
			if len(idx.TopPatterns) != 0 {
				t.Fatalf("per-task patterns must be stripped in rollup mode: %+v", idx)
			}
			if idx.Name == "" {
				t.Fatalf("headline must keep artifact name for search_artifact drill-in: %+v", idx)
			}
		}
	}
}

func TestMaybeBatchRollupSkipsWhenCollectorsDiffer(t *testing.T) {
	views := []taskView{
		makeLogTask("t1", "h1", "journal", "err <n>", "error", 5),
		makeLogTask("t2", "h2", "docker_logs", "err <n>", "error", 5),
	}
	_, _, applied, differ := maybeBatchRollup(views, rollupMaxHostsPerCluster)
	if applied {
		t.Fatalf("rollup must not apply when collectors differ")
	}
	if !differ {
		t.Fatalf("differing collectors should be reported so the caller can WARN")
	}
}

func TestMaybeBatchRollupSkipsSingleHost(t *testing.T) {
	views := []taskView{makeLogTask("t1", "h1", "journal", "err <n>", "error", 5)}
	_, _, applied, _ := maybeBatchRollup(views, rollupMaxHostsPerCluster)
	if applied {
		t.Fatalf("single-host result should not be rolled up")
	}
}

func TestTruncateRunes(t *testing.T) {
	if got := truncateRunes("short", 80); got != "short" {
		t.Fatalf("short string should be unchanged, got %q", got)
	}
	long := strings.Repeat("a", 100)
	got := truncateRunes(long, 80)
	if r := []rune(got); len(r) != 81 || r[80] != '…' {
		t.Fatalf("expected 80 runes + ellipsis, got %d runes", len([]rune(got)))
	}
}
