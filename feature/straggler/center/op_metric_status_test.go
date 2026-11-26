package center

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Computing-Availability-Tools/CATHelper/feature/straggler/profiling/detector"
)

func TestHandleOpMetricStatuses(t *testing.T) {
	c := newKPITestCenter(t)
	b := c.biz["biz"]
	b.triggering = true
	b.pendingToken = "round-1"
	d := b.Daemons[0]

	post := func(body string) (int, string) {
		rr := serveCenter(c, http.MethodPost, "/center/op_metric/biz/1.2.3.4:8080", body,
			map[string]string{"X-Match-Key": "secret"})
		return rr.Code, rr.Body.String()
	}

	// Legacy bare rank map is still accepted as an OK report.
	code, body := post(`{"0":{}}`)
	if code != http.StatusOK {
		t.Fatalf("legacy report status = %d body=%s", code, body)
	}
	if d.lastReportStatus != "ok" || d.lastOpMetric == nil {
		t.Fatalf("legacy report not stored: status=%q op=%v", d.lastReportStatus, d.lastOpMetric)
	}

	// Explicit failed report: stored, and the cached op_metric is cleared.
	d.lastOpMetric = detector.OpMetric{"0": &detector.OpMetricRank{}}
	code, body = post(`{"round_token":"round-1","status":"failed","reason":"no_db_after_analyse","stage":"analyse","ranks":null}`)
	if code != http.StatusOK {
		t.Fatalf("failed report status = %d body=%s", code, body)
	}
	if d.lastReportStatus != "failed" || d.lastReportReason != "no_db_after_analyse" || d.lastReportStage != "analyse" {
		t.Fatalf("failed report fields = %+v", d)
	}
	if d.lastOpMetric != nil {
		t.Fatalf("failed report kept stale op_metric: %+v", d.lastOpMetric)
	}

	// OK report stores ranks.
	code, body = post(`{"round_token":"round-1","status":"ok","ranks":{"0":{}}}`)
	if code != http.StatusOK || d.lastReportStatus != "ok" || d.lastOpMetric == nil {
		t.Fatalf("ok report not stored: code=%d status=%q op=%v body=%s", code, d.lastReportStatus, d.lastOpMetric, body)
	}

	// Busy/paused reports also clear cached data and count as reported.
	d.lastOpMetric = detector.OpMetric{"0": &detector.OpMetricRank{}}
	code, body = post(`{"round_token":"round-1","status":"busy","reason":"cycle_in_flight","busy_eta_ms":216000,"ranks":null}`)
	if code != http.StatusOK || d.lastReportStatus != "busy" || d.lastOpMetric != nil {
		t.Fatalf("busy report: code=%d status=%q op=%v body=%s", code, d.lastReportStatus, d.lastOpMetric, body)
	}
	if d.lastBusyEtaMs != 216000 {
		t.Fatalf("busy_eta_ms = %d, want 216000", d.lastBusyEtaMs)
	}
	d.lastOpMetric = detector.OpMetric{"0": &detector.OpMetricRank{}}
	code, body = post(`{"round_token":"round-1","status":"paused","reason":"paused","ranks":null}`)
	if code != http.StatusOK || d.lastReportStatus != "paused" || d.lastOpMetric != nil {
		t.Fatalf("paused report: code=%d status=%q op=%v body=%s", code, d.lastReportStatus, d.lastOpMetric, body)
	}

	// A report carrying another round's token is ignored.
	before := d.lastReportStatus
	code, body = post(`{"round_token":"old","status":"failed","reason":"x","stage":"collect","ranks":null}`)
	if code != http.StatusOK || !strings.Contains(body, "ignored") {
		t.Fatalf("stale token response = %d %s", code, body)
	}
	if d.lastReportStatus != before {
		t.Fatalf("stale report changed status from %q to %q", before, d.lastReportStatus)
	}

	// A report after the round closed is ignored.
	b.triggering = false
	code, body = post(`{"round_token":"round-1","status":"ok","ranks":{"0":{}}}`)
	if code != http.StatusOK || !strings.Contains(body, "ignored") {
		t.Fatalf("late report response = %d %s", code, body)
	}
}

func TestMarkMissingReportsDistinguishesReportedFailure(t *testing.T) {
	triggerAt := time.Date(2026, 11, 26, 10, 0, 0, 0, time.UTC)
	reportedFailed := &Daemon{
		IP: "10.0.0.1", Port: 8080, matchState: "healthy",
		lastReportAt: triggerAt.Add(time.Second), lastReportStatus: "failed",
	}
	missing := &Daemon{
		IP: "10.0.0.2", Port: 8080, matchState: "healthy",
		lastReportAt: triggerAt.Add(-time.Minute),
	}
	c := &Center{biz: map[string]*Business{}, logf: noopLogf}
	c.markMissingReports([]*Daemon{reportedFailed, missing}, triggerAt, "round-1")

	if reportedFailed.matchState != "reported_failed" {
		t.Fatalf("reported failed state = %q, want reported_failed", reportedFailed.matchState)
	}
	if missing.matchState != "report_failed" {
		t.Fatalf("missing state = %q, want report_failed", missing.matchState)
	}
}

func TestProbeRestoresTokenCapability(t *testing.T) {
	d := &Daemon{IP: "10.0.0.1", Port: 8080, Key: "secret", matchState: "healthy"}
	b := &Business{Name: "biz", Daemons: []*Daemon{d}}
	c := &Center{biz: map[string]*Business{}, logf: noopLogf}
	p := daemonProbe{
		b: b, d: d, key: "secret", healthOK: true,
		st: &matchStatusResp{Matched: true, ReportEtaMs: 156000, RoundTokenSupported: true, CollectWait: 60},
	}
	c.applyProbeLocked(p)
	if !d.tokenCapable {
		t.Fatalf("probe did not restore tokenCapable")
	}
	if d.reportEtaMs != 156000 {
		t.Fatalf("probe reportEtaMs = %d, want 156000", d.reportEtaMs)
	}
}

func TestReportTimeoutAuto(t *testing.T) {
	now := time.Now()
	c := &Center{}
	b := &Business{}

	d := &Daemon{reportEtaMs: 156000, reportEtaAt: now}
	if got := c.reportTimeoutLocked(b, []*Daemon{d}); got != 216 {
		t.Fatalf("eta 156s timeout = %d, want 216", got)
	}

	// Stale ETA falls back to the business's persisted value.
	d.reportEtaAt = now.Add(-2 * time.Hour)
	b.LastReportTimeoutSec = 300
	if got := c.reportTimeoutLocked(b, []*Daemon{d}); got != 300 {
		t.Fatalf("stale eta timeout = %d, want 300", got)
	}

	// No ETA and no persisted value -> 360 default.
	b.LastReportTimeoutSec = 0
	if got := c.reportTimeoutLocked(b, []*Daemon{d}); got != 360 {
		t.Fatalf("default timeout = %d, want 360", got)
	}

	// Clamp lower/upper bounds.
	d.reportEtaAt = now
	d.reportEtaMs = 1000
	if got := c.reportTimeoutLocked(b, []*Daemon{d}); got != 120 {
		t.Fatalf("low clamp = %d, want 120", got)
	}
	d.reportEtaMs = 5_000_000
	if got := c.reportTimeoutLocked(b, []*Daemon{d}); got != 1800 {
		t.Fatalf("high clamp = %d, want 1800", got)
	}
}

func TestAllDaemonsFailed(t *testing.T) {
	if !allDaemonsFailed(0, 3, 0) {
		t.Fatal("all non-ok reports must count as all-failed")
	}
	if allDaemonsFailed(1, 3, 0) {
		t.Fatal("one ok report must not count as all-failed")
	}
	if allDaemonsFailed(0, 3, 1) {
		t.Fatal("a missing daemon must not count as all-reported failure")
	}
	if allDaemonsFailed(0, 0, 0) {
		t.Fatal("empty business must not count as all-failed")
	}
}
