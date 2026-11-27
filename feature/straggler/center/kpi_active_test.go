package center

import (
	"testing"
	"time"
)

// The active gate is a two-class decision: active or not_active, with unknown
// when there are no fresh vLLM metrics. It requires every engine to have a
// latest running value >= 1 AND the node's aicore_util median >= 25%.
func TestClassifyLoadActiveGate(t *testing.T) {
	fresh := loadSnapshot{Fresh: true, Points: 10, MinEngineRunning: 1}
	if got := classifyLoad(fresh, 50); got != "active" {
		t.Fatalf("classifyLoad(fresh, 50) = %q, want active", got)
	}
	if got := classifyLoad(fresh, 24.9); got != "not_active" {
		t.Fatalf("classifyLoad(fresh, 24.9) = %q, want not_active", got)
	}
	if got := classifyLoad(fresh, 25); got != "active" {
		t.Fatalf("classifyLoad(fresh, 25) = %q, want active", got)
	}
	noRunning := loadSnapshot{Fresh: true, Points: 10, MinEngineRunning: 0.99}
	if got := classifyLoad(noRunning, 90); got != "not_active" {
		t.Fatalf("classifyLoad(min engine running < 1) = %q, want not_active", got)
	}
	if got := classifyLoad(loadSnapshot{}, 90); got != "unknown" {
		t.Fatalf("classifyLoad(no points) = %q, want unknown", got)
	}
	if got := classifyLoad(loadSnapshot{Fresh: false, Points: 10, MinEngineRunning: 2}, 90); got != "unknown" {
		t.Fatalf("classifyLoad(stale) = %q, want unknown", got)
	}
}

func TestLoadSnapshotMinEngineRunning(t *testing.T) {
	m := newMetricsStore()
	now := int64(10000)
	m.lastAt["biz"] = now
	m.series["biz"] = map[string]*engineMetrics{
		"e1": {points: []metricPoint{{Ts: now - 5, Running: 3}}},
		"e2": {points: []metricPoint{{Ts: now - 5, Running: 1}}},
	}
	if got := m.LoadSnapshot("biz", now).MinEngineRunning; got != 1 {
		t.Fatalf("MinEngineRunning = %v, want 1", got)
	}
	// A stale engine is ignored (not counted as 0): the fresh engine still runs.
	m.series["biz"]["e2"].points = []metricPoint{{Ts: now - 400, Running: 1}}
	if got := m.LoadSnapshot("biz", now).MinEngineRunning; got != 3 {
		t.Fatalf("MinEngineRunning with stale engine = %v, want 3", got)
	}
	// Engines can report at different timestamps inside the window: each
	// engine's own latest value counts, so one idle engine still makes the
	// business not_active even though it is not the latest scrape.
	m.series["biz"]["e2"].points = []metricPoint{{Ts: now - 10, Running: 0}}
	if got := m.LoadSnapshot("biz", now).MinEngineRunning; got != 0 {
		t.Fatalf("MinEngineRunning with in-window idle engine = %v, want 0", got)
	}
	// An engine that DID report at the latest timestamp with running=0 still
	// makes the business not_active.
	m.series["biz"]["e2"].points = []metricPoint{{Ts: now - 5, Running: 0}}
	if got := m.LoadSnapshot("biz", now).MinEngineRunning; got != 0 {
		t.Fatalf("MinEngineRunning with fresh zero engine = %v, want 0", got)
	}
}

func TestKPIReferenceWindowConstants(t *testing.T) {
	if kpiReferenceLimit != 50000 {
		t.Fatalf("kpiReferenceLimit = %d, want 50000", kpiReferenceLimit)
	}
	if kpiMinReferencePoints != 100 {
		t.Fatalf("kpiMinReferencePoints = %d, want 100", kpiMinReferencePoints)
	}
	if kpiSpaceMinDevices != 3 {
		t.Fatalf("kpiSpaceMinDevices = %d, want 3", kpiSpaceMinDevices)
	}
	if kpiSampleRetention != 48*time.Hour {
		t.Fatalf("kpiSampleRetention = %s, want 48h", kpiSampleRetention)
	}
	if kpiAnomalyRetention != 30*24*time.Hour {
		t.Fatalf("kpiAnomalyRetention = %s, want 30d", kpiAnomalyRetention)
	}
	if kpiEnableTimeDimension {
		t.Fatalf("kpiEnableTimeDimension = true, want false (space dimension only)")
	}
}
