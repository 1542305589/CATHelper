package center

import (
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// A manual trigger arriving while a round is in flight queues a next round and
// returns 202; the current round is unaffected.
func TestBusinessTriggerQueuesWhileRunning(t *testing.T) {
	c := newKPITestCenter(t)
	b := c.biz["biz"]
	b.triggering = true
	b.NextTrigger = time.Now().Add(time.Hour)

	rr := serveCenter(c, http.MethodPost, "/center/business/biz/trigger", "", nil)
	if rr.Code != http.StatusAccepted {
		t.Fatalf("trigger while running status = %d, body=%s", rr.Code, rr.Body.String())
	}
	if !b.pendingRound || b.pendingReason != "manual" || !b.queueIfBusyNext {
		t.Fatalf("pending = %v reason=%q queue=%v, want true/manual/true",
			b.pendingRound, b.pendingReason, b.queueIfBusyNext)
	}
	if b.NextTrigger.After(time.Now()) {
		t.Fatalf("pending NextTrigger = %s, want no later than now", b.NextTrigger)
	}
}

// Timer/KPI triggers that land mid-round merge into one queued reason and do not
// set the KPI cooldown until the queued round actually starts.
func TestTriggerBusinessModeQueuesBehindRunningRound(t *testing.T) {
	c := &Center{biz: map[string]*Business{}, logf: noopLogf, cfg: Config{DataDir: t.TempDir()}}
	b := &Business{Name: "biz", triggering: true, NextTrigger: time.Now().Add(time.Hour)}
	c.biz["biz"] = b

	if !c.triggerBusinessMode(b, true, "kpi") {
		t.Fatal("triggerBusinessMode did not report the round as queued")
	}
	if !b.pendingRound || b.pendingReason != "kpi" {
		t.Fatalf("pending = %v reason=%q, want true/kpi", b.pendingRound, b.pendingReason)
	}
	if !b.kpiProfilerCooldownUntil.IsZero() {
		t.Fatalf("KPI cooldown set before the round started: %s", b.kpiProfilerCooldownUntil)
	}

	if !c.triggerBusinessMode(b, true, "timer") {
		t.Fatal("second trigger was not queued")
	}
	if b.pendingReason != "kpi+timer" {
		t.Fatalf("merged pending reason = %q, want kpi+timer", b.pendingReason)
	}
}

// An earlier trigger while a queued round is waiting for busy daemons absorbs
// the queued reason and starts one round instead of leaving a second behind.
func TestTriggerBusinessModeAbsorbsPendingWhenIdle(t *testing.T) {
	c := &Center{biz: map[string]*Business{}, logf: noopLogf, cfg: Config{DataDir: t.TempDir()}}
	b := &Business{
		Name: "biz", pendingRound: true, pendingReason: "timer",
		queueIfBusyNext: true, NextTrigger: time.Now().Add(time.Hour),
	}
	c.biz["biz"] = b

	if c.triggerBusinessMode(b, true, "kpi") {
		t.Fatal("triggerBusinessMode queued instead of starting the pending round")
	}
	if b.pendingRound || b.pendingReason != "" {
		t.Fatalf("pending = %v reason=%q, want cleared", b.pendingRound, b.pendingReason)
	}
	if b.lastTriggerReason != "timer+kpi" {
		t.Fatalf("trigger reason = %q, want timer+kpi", b.lastTriggerReason)
	}
	if b.triggering {
		t.Fatal("round did not finish")
	}
}

func TestPauseClearsPendingRound(t *testing.T) {
	c := newKPITestCenter(t)
	b := c.biz["biz"]
	b.pendingRound = true
	b.pendingReason = "timer"
	b.queueIfBusyNext = true

	rr := serveCenter(c, http.MethodPost, "/center/business/biz/pause", "", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("pause status = %d, body=%s", rr.Code, rr.Body.String())
	}
	if b.pendingRound || b.pendingReason != "" || b.queueIfBusyNext {
		t.Fatalf("pending after pause = %v/%q/%v, want cleared", b.pendingRound, b.pendingReason, b.queueIfBusyNext)
	}
}

func TestMergeTriggerReason(t *testing.T) {
	if got := mergeTriggerReason("", "kpi"); got != "kpi" {
		t.Fatalf("empty+kpi = %q", got)
	}
	if got := mergeTriggerReason("kpi", ""); got != "kpi" {
		t.Fatalf("kpi+empty = %q", got)
	}
	if got := mergeTriggerReason("kpi", "kpi"); got != "kpi" {
		t.Fatalf("kpi+kpi = %q", got)
	}
	if got := mergeTriggerReason("timer+kpi", "kpi"); got != "timer+kpi" {
		t.Fatalf("timer+kpi + kpi = %q", got)
	}
	if got := mergeTriggerReason("timer", "kpi"); got != "timer+kpi" {
		t.Fatalf("timer+kpi = %q", got)
	}
}

// queue_if_busy=true must add the previous round's remaining busy time to the
// collection window (clamped to 1800s).
func TestQueueBusyTimeout(t *testing.T) {
	if got := queueBusyTimeout(216, 0); got != 216 {
		t.Fatalf("no busy eta timeout = %d, want 216", got)
	}
	if got := queueBusyTimeout(216, 156_000); got != 372 {
		t.Fatalf("busy eta timeout = %d, want 372", got)
	}
	if got := queueBusyTimeout(1700, 200_000); got != 1800 {
		t.Fatalf("busy eta high clamp = %d, want 1800", got)
	}
	if got := queueBusyTimeout(100, 0); got != 120 {
		t.Fatalf("base low clamp = %d, want 120", got)
	}
}

func TestMergeOpMetricReportsBusyEta(t *testing.T) {
	triggerAt := time.Now()
	d := &Daemon{
		IP: "10.0.0.1", Port: 8080, matchState: "healthy",
		lastReportAt:     triggerAt.Add(time.Second),
		lastReportStatus: "busy",
		lastBusyEtaMs:    216_000,
	}
	c := &Center{biz: map[string]*Business{}, logf: noopLogf}
	merged, ok, failed, missing, reported, _, maxBusy := c.mergeOpMetric([]*Daemon{d}, triggerAt, "round-1")
	if len(merged) != 0 || ok != 0 || len(failed) != 1 || len(missing) != 0 || reported != 1 {
		t.Fatalf("merge = %v/%d/%v/%v/%d, want empty/0/[d]/[]/1", merged, ok, failed, missing, reported)
	}
	if maxBusy != 216_000 {
		t.Fatalf("max busy eta = %d, want 216000", maxBusy)
	}
}

// cycle.json carries busy_eta_ms through to the history endpoint.
func TestCycleInfosCarriesBusyEta(t *testing.T) {
	dir := t.TempDir()
	cycleDir := filepath.Join(dir, "biz", "20261126-100000")
	if err := os.MkdirAll(cycleDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cycleDir, "cycle.json"),
		[]byte(`{"started_at":"2026-11-26T10:00:00Z","busy_eta_ms":216000,"report_timeout_sec":372}`), 0o644); err != nil {
		t.Fatal(err)
	}
	c := &Center{biz: map[string]*Business{}, logf: noopLogf, cfg: Config{DataDir: dir}}
	infos := c.cycleInfos("biz")
	if len(infos) != 1 {
		t.Fatalf("cycle infos = %d, want 1", len(infos))
	}
	if infos[0].BusyEtaMs != 216_000 || infos[0].ReportTimeoutSec != 372 {
		t.Fatalf("cycle info = busy %d timeout %d, want 216000/372", infos[0].BusyEtaMs, infos[0].ReportTimeoutSec)
	}
}
