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
	code, body = post(`{"round_token":"round-1","status":"failed","reason":"no_dump_after_collect","stage":"collect","ranks":null}`)
	if code != http.StatusOK {
		t.Fatalf("failed report status = %d body=%s", code, body)
	}
	if d.lastReportStatus != "failed" || d.lastReportReason != "no_dump_after_collect" || d.lastReportStage != "collect" {
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

	// A report carrying another round's token is ignored.
	code, body = post(`{"round_token":"old","status":"failed","reason":"x","stage":"collect","ranks":null}`)
	if code != http.StatusOK || !strings.Contains(body, "ignored") {
		t.Fatalf("stale token response = %d %s", code, body)
	}
	if d.lastReportStatus != "ok" {
		t.Fatalf("stale report changed status to %q", d.lastReportStatus)
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

func TestP0ReportTimeoutConstant(t *testing.T) {
	if p0ReportTimeout != 480*time.Second {
		t.Fatalf("p0ReportTimeout = %s, want 480s", p0ReportTimeout)
	}
}

func TestAllDaemonsFailed(t *testing.T) {
	if !allDaemonsFailed(0, 3, 3) {
		t.Fatal("all non-ok reports must count as all-failed")
	}
	if allDaemonsFailed(1, 3, 3) {
		t.Fatal("one ok report must not count as all-failed")
	}
	if allDaemonsFailed(0, 2, 3) {
		t.Fatal("a missing daemon must not count as all-reported failure")
	}
	if allDaemonsFailed(0, 0, 0) {
		t.Fatal("empty business must not count as all-failed")
	}
}
