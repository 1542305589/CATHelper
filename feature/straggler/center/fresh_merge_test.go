package center

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Computing-Availability-Tools/CATHelper/feature/straggler/profiling/detector"
)

func noopLogf(string, ...any) {}

// A daemon that did not report this round must not contribute its cached
// op_metric from an earlier round.
func TestMergeOpMetricSkipsStaleDaemons(t *testing.T) {
	triggerAt := time.Date(2026, 11, 25, 10, 0, 0, 0, time.UTC)
	stale := &Daemon{
		IP: "10.0.0.1", Port: 8080, matchState: "healthy",
		lastOpMetric: detector.OpMetric{"0": &detector.OpMetricRank{}},
		lastReportAt: triggerAt.Add(-time.Minute),
	}
	fresh := &Daemon{
		IP: "10.0.0.2", Port: 8080, matchState: "healthy",
		lastOpMetric: detector.OpMetric{
			"1": &detector.OpMetricRank{},
			"2": &detector.OpMetricRank{},
		},
		lastReportAt: triggerAt.Add(time.Second),
	}
	c := &Center{biz: map[string]*Business{}, logf: noopLogf}
	b := &Business{Name: "biz", Daemons: []*Daemon{stale, fresh}}

	merged, freshDaemons := c.mergeOpMetric(b, triggerAt)
	if freshDaemons != 1 {
		t.Fatalf("fresh daemons = %d, want 1", freshDaemons)
	}
	if len(merged) != 2 {
		t.Fatalf("merged ranks = %d, want 2 (only daemon B)", len(merged))
	}
	if _, ok := merged["0"]; ok {
		t.Fatalf("stale rank 0 from daemon A was merged")
	}
	if _, ok := merged["1"]; !ok {
		t.Fatalf("fresh rank 1 from daemon B missing")
	}
	if _, ok := merged["2"]; !ok {
		t.Fatalf("fresh rank 2 from daemon B missing")
	}
}

// No daemon reported this round: the merge must be empty so the caller marks
// the round failed instead of re-detecting cached data.
func TestMergeOpMetricNoFreshReturnsEmpty(t *testing.T) {
	triggerAt := time.Date(2026, 11, 25, 10, 0, 0, 0, time.UTC)
	c := &Center{biz: map[string]*Business{}, logf: noopLogf}
	b := &Business{Name: "biz", Daemons: []*Daemon{
		{IP: "10.0.0.1", Port: 8080, matchState: "report_failed",
			lastOpMetric: detector.OpMetric{"0": &detector.OpMetricRank{}},
			lastReportAt: triggerAt.Add(-time.Minute)},
		{IP: "10.0.0.2", Port: 8080, matchState: "healthy",
			lastOpMetric: nil, lastReportAt: time.Time{}},
	}}

	merged, freshDaemons := c.mergeOpMetric(b, triggerAt)
	if freshDaemons != 0 {
		t.Fatalf("fresh daemons = %d, want 0", freshDaemons)
	}
	if len(merged) != 0 {
		t.Fatalf("merged ranks = %d, want 0", len(merged))
	}
}

// A failed round records world_size=0 / empty summary / failure_reason and
// writes NO detection artifacts.
func TestWriteFailedCycleRecordsReasonAndNoArtifacts(t *testing.T) {
	dir := t.TempDir()
	c := &Center{cfg: Config{DataDir: dir}, biz: map[string]*Business{}, logf: noopLogf}
	b := &Business{Name: "biz"}
	startedAt := time.Date(2026, 11, 25, 10, 0, 0, 0, time.UTC)
	const reason = "no fresh op_metric in round"
	c.writeFailedCycle(b, startedAt, 1234, "timer", reason)

	cycleDir := filepath.Join(dir, "biz", startedAt.Format("20060102-150405"))
	raw, err := os.ReadFile(filepath.Join(cycleDir, "cycle.json"))
	if err != nil {
		t.Fatalf("read cycle.json: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("parse cycle.json: %v", err)
	}
	if failed, _ := m["failed"].(bool); !failed {
		t.Fatalf("failed = %v, want true", m["failed"])
	}
	if got, _ := m["failure_reason"].(string); got != reason {
		t.Fatalf("failure_reason = %q, want %q", got, reason)
	}
	if ws, _ := m["world_size"].(float64); ws != 0 {
		t.Fatalf("world_size = %v, want 0", m["world_size"])
	}
	if s, ok := m["summary"]; !ok || s != nil {
		t.Fatalf("summary = %v, want nil", m["summary"])
	}

	for _, name := range []string{
		"op_metric.json",
		"straggler_output.json",
		filepath.Join("analysis_result", "detection_report.log"),
	} {
		if _, err := os.Stat(filepath.Join(cycleDir, name)); !os.IsNotExist(err) {
			t.Fatalf("%s must not exist for a failed round (err=%v)", name, err)
		}
	}

	// The history endpoint exposes the failure reason.
	infos := c.cycleInfos("biz")
	if len(infos) != 1 {
		t.Fatalf("cycleInfos = %d entries, want 1", len(infos))
	}
	if !infos[0].Failed || infos[0].FailureReason != reason {
		t.Fatalf("history failed=%v failure_reason=%q", infos[0].Failed, infos[0].FailureReason)
	}
}
