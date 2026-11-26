package center

import (
	"bytes"
	"crypto/rand"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Computing-Availability-Tools/CATHelper/feature/straggler/config"
	"github.com/Computing-Availability-Tools/CATHelper/feature/straggler/profiling/detector"
	"github.com/Computing-Availability-Tools/CATHelper/feature/straggler/report"
	"github.com/Computing-Availability-Tools/CATHelper/feature/straggler/utils"
)

const (
	probeInterval   = 5 * time.Second
	probeFailLimit  = 3  // consecutive healthz failures → 断连
	reportBufferSec = 60 // fixed buffer added to max collect-wait
)

// daemonProbe is one daemon's probe target and result. The center lock is only
// held while snapshotting targets and applying results, never during HTTP I/O.
type daemonProbe struct {
	b        *Business
	d        *Daemon
	key      string
	healthOK bool
	st       *matchStatusResp
	stErr    error
}

// heartbeatLoop probes every daemon (healthz + match_status) forever. Probes run
// concurrently outside c.mu so one slow/hung daemon cannot block the web UI or
// KPI queries.
func (c *Center) heartbeatLoop() {
	ticker := time.NewTicker(probeInterval)
	defer ticker.Stop()
	const probeConcurrency = 8
	for range ticker.C {
		c.mu.Lock()
		probes := make([]daemonProbe, 0)
		for _, b := range c.biz {
			for _, d := range b.Daemons {
				probes = append(probes, daemonProbe{b: b, d: d, key: d.Key})
			}
		}
		c.mu.Unlock()

		sem := make(chan struct{}, probeConcurrency)
		var wg sync.WaitGroup
		for i := range probes {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				sem <- struct{}{}
				defer func() { <-sem }()
				p := &probes[i]
				if !daemonHealth(p.d) {
					return
				}
				p.healthOK = true
				if p.key == "" {
					return
				}
				p.st, p.stErr = daemonMatchStatus(p.d, p.key)
			}(i)
		}
		wg.Wait()

		c.mu.Lock()
		rematch := make([]daemonProbe, 0)
		for i := range probes {
			p := &probes[i]
			if !c.daemonPresentLocked(p.b, p.d) || p.key != p.d.Key {
				continue
			}
			if c.applyProbeLocked(*p) {
				rematch = append(rematch, *p)
			}
		}
		c.mu.Unlock()
		for _, p := range rematch {
			go c.tryMatch(p.b, p.d)
		}
	}
}

// scheduleLoop triggers each business when its interval elapses (paused
// businesses are skipped).
func (c *Center) scheduleLoop() {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for range ticker.C {
		c.mu.Lock()
		changed := false
		for _, b := range c.biz {
			if b.Paused {
				continue
			}
			if b.pendingRound {
				if b.NextTrigger.IsZero() {
					b.NextTrigger = time.Now()
					changed = true
				}
				if time.Now().Before(b.NextTrigger) || b.triggering || !c.businessReadyLocked(b) {
					continue
				}
				reason := b.pendingReason
				b.pendingRound = false
				b.pendingReason = ""
				b.queueIfBusyNext = true
				changed = true
				c.logf("business %s: starting queued round (reason=%s)", b.Name, reason)
				go c.triggerBusinessMode(b, true, reason)
				continue
			}
			interval := c.businessInterval(b)
			if b.NextTrigger.IsZero() {
				b.NextTrigger = time.Now().Add(interval)
				changed = true
				continue
			}
			if time.Now().Before(b.NextTrigger) {
				continue
			}
			// Deadline elapsed (normal tick or restart catch-up): fire once and
			// re-anchor. When the business is not ready yet, keep the expired
			// deadline so the single catch-up happens as soon as daemons re-match.
			if !c.businessReadyLocked(b) {
				continue
			}
			if b.triggering {
				// The timer fired while a round is in flight: queue a next round
				// instead of dropping this tick.
				go c.triggerBusinessMode(b, true, "timer")
				continue
			}
			b.NextTrigger = time.Now().Add(interval)
			changed = true
			go c.triggerBusiness(b)
		}
		if changed {
			c.save()
		}
		c.mu.Unlock()
	}
}

// businessReadyLocked reports whether every daemon is healthy & matched (a
// frozen business is skipped until the user reconnects/removes the bad daemon).
func (c *Center) businessReadyLocked(b *Business) bool {
	if len(b.Daemons) == 0 {
		return false
	}
	for _, d := range b.Daemons {
		if d.matchState != "healthy" {
			return false
		}
	}
	return true
}

// businessInterval returns a business's trigger period, falling back to the
// center default when unset/non-positive. Both scheduleLoop and triggerBusiness
// must use this so nextTrigger is never anchored in the past.
func (c *Center) businessInterval(b *Business) time.Duration {
	if b.IntervalSec > 0 {
		return time.Duration(b.IntervalSec) * time.Second
	}
	return c.cfg.Interval
}

// ---------------------------------------------------------------------------
// Matching
// ---------------------------------------------------------------------------

func randomKey() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// matchDaemonLocked generates a key and matches a daemon to this center. The
// daemon replies with its collect-wait (used for the report timeout).
func (c *Center) matchDaemonLocked(b *Business, d *Daemon) error {
	d.Key = randomKey()
	// Persist the key immediately so a center restart re-authenticates with the
	// SAME key (not an empty or freshly-random one). Callers hold c.mu.
	c.save()
	body := map[string]any{
		"center_addr":  c.selfURL(),
		"key":          d.Key,
		"business":     b.Name,
		"daemon":       d.Addr(),
		"interval_sec": int64(c.businessInterval(b).Seconds()),
	}
	resp, err := doJSON(http.MethodPost, d.BaseURL()+"/daemon/match", body, "")
	if err != nil {
		d.Key = ""
		c.save()
		return err
	}
	cw, _ := resp["collect_wait"].(float64)
	d.collectWait = int64(cw)
	if capable, ok := resp["round_token_supported"].(bool); ok {
		d.tokenCapable = capable
	}
	if eta, ok := resp["report_eta_ms"].(float64); ok && eta > 0 {
		d.reportEtaMs = int64(eta)
		d.reportEtaAt = time.Now()
	}
	d.matchState = "healthy"
	d.healthy = true
	d.failCount = 0
	return nil
}

// unmatchDaemonLocked asks a matched daemon to unmatch (best-effort).
func (c *Center) unmatchDaemonLocked(d *Daemon) {
	if d.Key == "" {
		return
	}
	_, _ = doJSON(http.MethodPost, d.BaseURL()+"/daemon/unmatch", nil, d.Key)
	d.Key = ""
	d.matchState = ""
	d.healthy = false
	d.lastOpMetric = nil
	d.lastReportAt = time.Time{}
}

// ---------------------------------------------------------------------------
// Probing (healthz + match_status → 4-state classification)
// ---------------------------------------------------------------------------

// daemonPresentLocked reports whether b still owns d after an unlocked probe.
func (c *Center) daemonPresentLocked(b *Business, d *Daemon) bool {
	if c.biz[b.Name] != b {
		return false
	}
	for _, cur := range b.Daemons {
		if cur == d {
			return true
		}
	}
	return false
}

// applyProbeLocked applies one probe result under c.mu. It returns true when the
// daemon should be auto-rematched; the caller must launch that outside the lock.
func (c *Center) applyProbeLocked(p daemonProbe) bool {
	d := p.d
	if !p.healthOK {
		d.failCount++
		if d.failCount >= probeFailLimit {
			d.healthy = false
			d.matchState = "" // 断连
		}
		return false
	}
	d.failCount = 0
	d.healthy = true

	if p.key == "" {
		d.matchState = "unmatched"
		return false
	}
	if p.stErr != nil {
		d.matchState = ""
		return false
	}
	switch {
	case p.st.Matched:
		d.matchState = "healthy"
		// collect_wait is runtime-only; recover it on every probe so a center
		// restart (which drops it) re-learns it without a full re-match.
		if p.st.CollectWait > 0 {
			d.collectWait = p.st.CollectWait
		}
		// Surface the daemon's current iterations so the console can show
		// drift (0 = old build / unknown).
		if p.st.Iterations > 0 {
			d.reportedIterations = p.st.Iterations
		}
		if p.st.ReportEtaMs > 0 {
			d.reportEtaMs = p.st.ReportEtaMs
			d.reportEtaAt = time.Now()
		}
		d.tokenCapable = p.st.RoundTokenSupported
	case p.st.Other:
		d.matchState = "other"
		return samePort(p.st.Center, c.cfg.Port)
	default:
		d.matchState = "unmatched"
		// A center restart makes the daemon drop its match while the center
		// still has the old key. Re-match only in that case: d.Key == "" means
		// the user intentionally unmatched it, so leave it alone.
		return d.Key != "" && p.st != nil && p.st.State == "running"
	}
	return false
}

func daemonHealth(d *Daemon) bool {
	resp, err := httpGet(d.BaseURL() + "/healthz")
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64))
	return resp.StatusCode == http.StatusOK && strings.TrimSpace(string(body)) == "ok"
}

type matchStatusResp struct {
	State               string `json:"state"`
	Matched             bool   `json:"matched"`
	Other               bool   `json:"other"`
	Center              string `json:"center"`
	CollectWait         int64  `json:"collect_wait"`
	Iterations          int    `json:"iterations"`
	ReportEtaMs         int64  `json:"report_eta_ms"`
	RoundTokenSupported bool   `json:"round_token_supported"`
}

func samePort(raw string, port int) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Port() == "" {
		return false
	}
	return u.Port() == strconv.Itoa(port)
}

func daemonMatchStatus(d *Daemon, key string) (*matchStatusResp, error) {
	var st matchStatusResp
	err := getJSON(d.BaseURL()+"/daemon/match_status", key, &st)
	return &st, err
}

// ---------------------------------------------------------------------------
// Trigger + merged detection
// ---------------------------------------------------------------------------

// triggerBusiness triggers every healthy daemon, waits for their reports (or
// the max-collect-wait+60s timeout), merges and re-runs detection. It tracks the
// business's cycle counters and next trigger, and re-anchors the schedule.
// Single-flight: a concurrent trigger for the same business queues one next
// round (reason merged) and returns true.
func (c *Center) triggerBusiness(b *Business) bool {
	return c.triggerBusinessMode(b, true, "timer")
}

// triggerBusinessFromKPI runs a KPI-triggered profiler round and refreshes the
// business's normal profiler schedule so the timer does not fire immediately after.
func (c *Center) triggerBusinessFromKPI(b *Business) bool {
	return c.triggerBusinessMode(b, true, "kpi")
}

func (c *Center) triggerBusinessMode(b *Business, reschedule bool, reason string) bool {
	triggerAt := time.Now()

	c.mu.Lock()
	if b.triggering {
		b.pendingRound = true
		b.pendingReason = mergeTriggerReason(b.pendingReason, reason)
		b.queueIfBusyNext = true
		if b.NextTrigger.IsZero() || b.NextTrigger.After(time.Now()) {
			b.NextTrigger = time.Now()
		}
		c.logf("business %s: round in flight; queued next round (reason=%s)", b.Name, b.pendingReason)
		c.save()
		c.mu.Unlock()
		return true
	}
	// A queued round is waiting for the daemons to become idle. An earlier
	// trigger starts it now and absorbs the queued reason instead of leaving a
	// second round behind.
	if b.pendingRound {
		reason = mergeTriggerReason(b.pendingReason, reason)
		b.pendingRound = false
		b.pendingReason = ""
		b.queueIfBusyNext = true
	}
	b.triggering = true
	b.lastTriggerReason = reason
	if strings.Contains(reason, "kpi") {
		// Cooldown starts when the KPI-triggered round actually starts, not when
		// the anomaly was recorded (it may be queued behind another round).
		b.kpiProfilerCooldownUntil = triggerAt.Add(10 * time.Minute)
	}
	// Round token: daemons echo it back with their report so a late report from
	// an older round can never be mistaken for this round's data.
	b.pendingToken = strconv.FormatInt(triggerAt.UnixNano(), 10)
	token := b.pendingToken
	// The center's business iteration count is sent with EVERY trigger; it does
	// not depend on (and does not modify) the daemon's saved self-run value.
	iterations := businessIterations(b)
	if b.progress == nil {
		b.progress = newProgressLog()
	}
	prog := b.progress
	round := b.CyclesTotal + b.CyclesFailed + 1

	expected := make([]*Daemon, 0, len(b.Daemons))
	var addrs []string
	for _, d := range b.Daemons {
		if d.matchState != "healthy" {
			continue
		}
		expected = append(expected, d)
		addrs = append(addrs, d.Addr())
		d.lastReportAt = time.Time{}
		d.lastReportStatus = ""
		d.lastReportReason = ""
		d.lastReportStage = ""
		d.lastOpMetric = nil
		d.lastBusyEtaMs = 0
	}
	healthy := len(expected)
	// queue_if_busy is consumed per round; the next round after a busy report
	// asks daemons to queue instead of reporting busy. Its timeout must include
	// the previous round's remaining busy time.
	queueIfBusy := b.queueIfBusyNext
	b.queueIfBusyNext = false
	busyEtaPrev := b.lastBusyEtaMs
	timeoutSec := c.reportTimeoutLocked(b, expected)
	if queueIfBusy {
		timeoutSec = queueBusyTimeout(timeoutSec, busyEtaPrev)
		c.logf("business %s: next round queue_if_busy (timeout=%ds, prev busy eta=%dms)", b.Name, timeoutSec, busyEtaPrev)
	}
	b.LastReportTimeoutSec = timeoutSec
	timeout := time.Duration(timeoutSec) * time.Second
	c.mu.Unlock()

	prog.begin(round, triggerAt)
	prog.step("业务 %s 检测开始（%d 个已匹配守护进程）", b.Name, healthy)
	prog.step("触发守护进程: %s", strings.Join(addrs, ", "))

	for _, d := range expected {
		go c.triggerDaemon(d, token, iterations, queueIfBusy, timeoutSec)
	}

	prog.step("等待 op_metric 回传（超时 %s）", timeout)
	deadline := time.Now().Add(timeout)
	reported := -1
	collected := false
	for time.Now().Before(deadline) {
		c.mu.Lock()
		done := true
		n := 0
		for _, d := range expected {
			if !roundReported(d, triggerAt, token) {
				done = false
			} else {
				n++
			}
		}
		c.mu.Unlock()
		if n != reported {
			reported = n
			prog.step("已回传 op_metric: %d/%d", n, healthy)
		}
		if done {
			collected = true
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	timedOut := !collected

	c.markMissingReports(expected, triggerAt, token)

	merged, okCount, failedDaemons, missingDaemons, reportedDaemons, reportLatencyMs, maxBusyEtaMs := c.mergeOpMetric(expected, triggerAt, token)
	meta := roundMeta{
		ReportTimeoutSec: timeoutSec,
		ReportedDaemons:  reportedDaemons,
		FailedDaemons:    failedDaemons,
		MissingDaemons:   missingDaemons,
		Partial:          okCount > 0 && (len(failedDaemons) > 0 || len(missingDaemons) > 0),
		ReportLatencyMs:  reportLatencyMs,
		BusyEtaMs:        maxBusyEtaMs,
	}
	durationMs := time.Since(triggerAt).Milliseconds()
	prog.step("合并本轮上报的 op_metric: %d 个 rank（%d/%d 个守护进程成功）", len(merged), okCount, healthy)

	var failed bool
	failureReason := ""
	if okCount == 0 {
		if allDaemonsFailed(okCount, healthy, len(missingDaemons)) {
			c.logf("business %s: all daemons reported failure", b.Name)
			prog.step("本轮全部守护进程上报失败，判定本轮失败")
			failureReason = "all daemons reported failure"
		} else {
			c.logf("business %s: no op_metric reported in this round", b.Name)
			prog.step("本轮无 op_metric 上报，判定本轮失败")
			failureReason = "no op_metric reported in round"
		}
		failed = true
	} else {
		prog.step("跨节点合并检测中 ...")
		if err := c.detectAndStore(b, merged, triggerAt, durationMs, iterations, meta); err != nil {
			c.logf("business %s: detect failed: %v", b.Name, err)
			prog.step("检测失败: %v", err)
			failed = true
			failureReason = "detection failed: " + err.Error()
		}
	}

	c.mu.Lock()
	if failed {
		b.CyclesFailed++
		c.writeFailedCycle(b, triggerAt, durationMs, reason, failureReason, iterations, meta)
	} else {
		b.CyclesTotal++
	}
	b.lastBusyEtaMs = maxBusyEtaMs
	sawBusy := maxBusyEtaMs > 0
	for _, d := range expected {
		if d.lastReportStatus == "busy" {
			sawBusy = true
			break
		}
	}
	if sawBusy || b.pendingRound {
		b.queueIfBusyNext = true
	}
	now := time.Now()
	// busy_eta_ms was measured relative to the round start; the daemon is idle
	// at triggerAt+busy_eta (plus a small buffer). Detection may finish much
	// later, so compute the absolute idle time instead of now+busy_eta.
	busyUntil := triggerAt.Add(time.Duration(maxBusyEtaMs)*time.Millisecond + 5*time.Second)
	if maxBusyEtaMs > 0 && busyUntil.Before(now) {
		busyUntil = now
	}
	if b.pendingRound {
		// The pending round starts via scheduleLoop once the busy daemon is idle.
		if maxBusyEtaMs > 0 {
			b.NextTrigger = busyUntil
		} else {
			b.NextTrigger = now
		}
	} else {
		next := time.Time{}
		if timedOut {
			next = now.Add(c.businessInterval(b))
		} else if reschedule {
			next = triggerAt.Add(c.businessInterval(b))
		}
		if maxBusyEtaMs > 0 {
			if next.IsZero() || busyUntil.After(next) {
				next = busyUntil
			}
		}
		if !next.IsZero() {
			b.NextTrigger = next
		}
	}
	b.triggering = false
	c.save()
	c.mu.Unlock()

	if failed {
		prog.step("本轮结束（失败，耗时 %dms）", durationMs)
	} else {
		prog.step("本轮结束（耗时 %dms）", durationMs)
	}
	prog.finish()
	// Persist this round's stage log under its result dir so history can show it
	// (the in-memory log only covers the current/last round).
	_ = prog.save(filepath.Join(c.cfg.DataDir, b.Name, triggerAt.Format("20060102-150405"), "progress.json"))
	return false
}

// mergeTriggerReason combines queued round reasons without duplicating a part.
func mergeTriggerReason(a, b string) string {
	if a == "" {
		return b
	}
	if b == "" || a == b {
		return a
	}
	for _, part := range strings.Split(a, "+") {
		if part == b {
			return a
		}
	}
	return a + "+" + b
}

// markMissingReports flags daemons that failed to report within the round's
// deadline (distinct from "断连": healthz is fine but the daemon lost its match
// or hung). The next probe re-classifies them automatically once they recover.
func (c *Center) markMissingReports(expected []*Daemon, triggerAt time.Time, token string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, d := range expected {
		if !roundReported(d, triggerAt, token) {
			d.matchState = "report_failed"
			continue
		}
		// The daemon answered this round, but explicitly reported failure.
		if d.lastReportStatus != "" && d.lastReportStatus != "ok" {
			d.matchState = "reported_failed"
		}
	}
}

// roundReported reports whether d's latest report belongs to the round started
// at triggerAt. Daemons without a token (old builds) fall back to a pure
// freshness check; new daemons must echo the round token.
func roundReported(d *Daemon, triggerAt time.Time, token string) bool {
	if d.lastReportAt.Before(triggerAt) {
		return false
	}
	if token != "" && d.tokenCapable && d.lastRoundToken != token {
		return false
	}
	return true
}

func (c *Center) triggerDaemon(d *Daemon, token string, iterations int, queueIfBusy bool, timeoutSec int) {
	body := map[string]any{
		"round_token":        token,
		"iterations":         iterations,
		"queue_if_busy":      queueIfBusy,
		"report_timeout_sec": timeoutSec,
	}
	_, _ = doJSON(http.MethodPost, d.BaseURL()+"/daemon/trigger", body, d.Key)
}

// daemonReportEtaLocked returns a daemon's fresh report ETA (0 if unknown/stale).
func (c *Center) daemonReportEtaLocked(d *Daemon) int64 {
	if d.reportEtaMs <= 0 {
		return 0
	}
	if !d.reportEtaAt.IsZero() && time.Since(d.reportEtaAt) > time.Hour {
		return 0
	}
	return d.reportEtaMs
}

// healthyDaemons filters the trigger/status candidate set. Status and interval
// validation use the same healthy set as the actual trigger snapshot.
func healthyDaemons(daemons []*Daemon) []*Daemon {
	out := make([]*Daemon, 0, len(daemons))
	for _, d := range daemons {
		if d.matchState == "healthy" {
			out = append(out, d)
		}
	}
	return out
}

// maxReportEtaLocked returns the largest fresh successful report ETA (ms) across
// daemons; a value older than one hour is treated as unknown. Caller holds c.mu.
func (c *Center) maxReportEtaLocked(daemons []*Daemon) int64 {
	maxEta := int64(0)
	for _, d := range daemons {
		if eta := c.daemonReportEtaLocked(d); eta > maxEta {
			maxEta = eta
		}
	}
	return maxEta
}

// reportTimeoutLocked computes the automatic collection window:
// max(eta)+60s, clamped to [120, 1800], falling back to the business's persisted
// value and then to the 360s default. Caller holds c.mu.
func (c *Center) reportTimeoutLocked(b *Business, daemons []*Daemon) int {
	if eta := c.maxReportEtaLocked(daemons); eta > 0 {
		return clampReportTimeout(int(eta/1000) + reportEtaBufferSec)
	}
	if b != nil && b.LastReportTimeoutSec > 0 {
		return clampReportTimeout(b.LastReportTimeoutSec)
	}
	return defaultReportTimeoutSec
}

// queueBusyTimeout enlarges the current round's window for a next round that
// must wait for a busy daemon to finish: base + remaining busy time, clamped.
func queueBusyTimeout(base int, busyEtaMs int64) int {
	if busyEtaMs <= 0 {
		return clampReportTimeout(base)
	}
	return clampReportTimeout(base + int(busyEtaMs/1000))
}

func clampReportTimeout(v int) int {
	if v < minReportTimeoutSec {
		return minReportTimeoutSec
	}
	if v > maxReportTimeoutSec {
		return maxReportTimeoutSec
	}
	return v
}

// businessIterations normalizes a business's iterations (0 = legacy/default).
func businessIterations(b *Business) int {
	if b.Iterations > 0 {
		return b.Iterations
	}
	return defaultBusinessIterations
}

// allDaemonsFailed reports whether every expected daemon answered this round
// with a non-ok status (failed/busy/paused), i.e. no op_metric is available and
// no daemon is missing.
func allDaemonsFailed(okCount, healthy, missingCount int) bool {
	return healthy > 0 && okCount == 0 && missingCount == 0
}

// mergeOpMetric merges only the OK reports from the round's expected daemons.
// A daemon that reported failed/busy/paused (or did not report at all) is
// skipped and returned in failed, so its cached op_metric can never be reused.
// It returns (merged, okCount, failedAddrs, missingAddrs, reportedCount,
// maxReportLatencyMs, maxBusyEtaMs).
func (c *Center) mergeOpMetric(expected []*Daemon, triggerAt time.Time, token string) (detector.OpMetric, int, []string, []string, int, int64, int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	merged := make(detector.OpMetric)
	okCount := 0
	reported := 0
	var failed []string
	var missing []string
	var maxLatencyMs int64
	var maxBusyEtaMs int64
	for _, d := range expected {
		if !roundReported(d, triggerAt, token) {
			missing = append(missing, d.Addr())
			c.logf("skip missing op_metric: daemon=%s lastReportAt=%s triggerAt=%s token=%q",
				d.Addr(), d.lastReportAt.UTC().Format(time.RFC3339), triggerAt.UTC().Format(time.RFC3339), d.lastRoundToken)
			continue
		}
		reported++
		if lat := d.lastReportAt.Sub(triggerAt).Milliseconds(); lat > maxLatencyMs {
			maxLatencyMs = lat
		}
		status := d.lastReportStatus
		if status == "" {
			status = "ok" // legacy bare map / {ranks} envelope
		}
		if status != "ok" || d.lastOpMetric == nil {
			failed = append(failed, d.Addr())
			if status == "busy" && d.lastBusyEtaMs > maxBusyEtaMs {
				maxBusyEtaMs = d.lastBusyEtaMs
			}
			c.logf("skip non-ok op_metric: daemon=%s status=%s reason=%s stage=%s token=%q",
				d.Addr(), status, d.lastReportReason, d.lastReportStage, d.lastRoundToken)
			continue
		}
		okCount++
		for rank, r := range d.lastOpMetric {
			merged[rank] = r
		}
	}
	return merged, okCount, failed, missing, reported, maxLatencyMs, maxBusyEtaMs
}

// detectAndStore runs detection on the merged op_metric and stores the result
// under the business's results dir (same shape as a single daemon's output). The
// op_metric is materialized to a temp dir and re-used through the existing
// file-backed detection path, so host-level (slow-CPU / cross-node) detection
// and the report's cross-node sections work exactly as in a daemon.
func (c *Center) detectAndStore(b *Business, op detector.OpMetric, startedAt time.Time, durationMs int64, iterations int, meta roundMeta) error {
	// Detection uses process-wide config globals (FilePath + thresholds), so
	// serialize it across businesses to avoid cross-business races.
	c.detectMu.Lock()
	defer c.detectMu.Unlock()

	tmp, err := restoreOpMetric(op)
	if err != nil {
		return fmt.Errorf("restore op_metric: %w", err)
	}
	defer os.RemoveAll(tmp)

	config.FilePath = tmp
	config.Apply(b.Thresholds)

	parallels, validRanks := detector.GetCurDetectionInfo(tmp)
	if len(validRanks) == 0 {
		return fmt.Errorf("no valid ranks")
	}
	stepData := detector.GetCurJobLastStepData(validRanks)
	result := detector.DelimitDetection(stepData, parallels, validRanks)
	status := detector.CategoryStatus(stepData, parallels, validRanks)
	if detector.AllNoData(status) {
		return fmt.Errorf("no valid profiler data (no_data)")
	}
	nodeOut, err := utils.BuildNodeResult(result, parallels, nil, status)
	if err != nil {
		return fmt.Errorf("build node result: %w", err)
	}
	c.mu.Lock()
	addrs := make([]string, 0, len(b.Daemons))
	for _, d := range b.Daemons {
		addrs = append(addrs, d.Addr())
	}
	c.mu.Unlock()
	source := strings.Join(addrs, ",")
	reportText := report.GenerateReport(stepData, parallels, validRanks, result, source)

	summary := map[string]int{
		"cal":  len(result["cal"]),
		"comm": len(result["comm"]),
		"cpu":  len(result["cpu"]),
	}
	if b.progress != nil {
		b.progress.step("检测完成: 慢计算 %d / 慢通信 %d / 慢CPU %d",
			summary["cal"], summary["comm"], summary["cpu"])
	}

	dir := startedAt.Format("20060102-150405")
	if err := c.writeBusinessResult(b, dir, nodeOut, reportText, op, summary, status, iterations, meta, startedAt, durationMs, len(op)); err != nil {
		return fmt.Errorf("store result: %w", err)
	}
	c.logf("business %s: detection done (%d ranks, %d cal anomalies)", b.Name, len(validRanks), len(result["cal"]))
	return nil
}

// restoreOpMetric materializes an in-memory op_metric into a temp dir laid out
// like a daemon's op_metric/ (group_info/host_info/npu_info JSON + global_rank
// CSV) so the file-backed detector can consume it.
func restoreOpMetric(op detector.OpMetric) (string, error) {
	tmp, err := os.MkdirTemp("", "center_opmetric_")
	if err != nil {
		return "", err
	}
	metricDir := filepath.Join(tmp, "op_metric")
	if err := os.MkdirAll(metricDir, 0o755); err != nil {
		return "", err
	}
	for rankStr, r := range op {
		if r == nil {
			continue
		}
		if r.GroupInfo != nil {
			if data, err := json.Marshal(r.GroupInfo); err == nil {
				_ = os.WriteFile(filepath.Join(metricDir, "group_info_"+rankStr+".json"), data, 0o644)
			}
		}
		if r.HostInfo != nil {
			if data, err := json.Marshal(r.HostInfo); err == nil {
				_ = os.WriteFile(filepath.Join(metricDir, "host_info_"+rankStr+".json"), data, 0o644)
			}
		}
		if r.NpuInfo != nil {
			if data, err := json.Marshal(r.NpuInfo); err == nil {
				_ = os.WriteFile(filepath.Join(metricDir, "npu_info_"+rankStr+".json"), data, 0o644)
			}
		}
		if r.GlobalRank != nil {
			_ = writeGlobalRankCSV(filepath.Join(metricDir, "global_rank_"+rankStr+".csv"), r.GlobalRank)
		}
	}
	return tmp, nil
}

// writeGlobalRankCSV writes a global_rank value (single object or array) as a
// CSV with the object keys as header, matching the daemon's global_rank_{N}.csv.
func writeGlobalRankCSV(path string, g any) error {
	var rows []map[string]any
	switch v := g.(type) {
	case map[string]any:
		rows = []map[string]any{v}
	case []any:
		for _, item := range v {
			if m, ok := item.(map[string]any); ok {
				rows = append(rows, m)
			}
		}
	}
	if len(rows) == 0 {
		return nil
	}
	var cols []string
	seen := make(map[string]bool)
	for _, row := range rows {
		for k := range row {
			if !seen[k] {
				seen[k] = true
				cols = append(cols, k)
			}
		}
	}
	var buf bytes.Buffer
	w := csv.NewWriter(&buf)
	if err := w.Write(cols); err != nil {
		return err
	}
	for _, row := range rows {
		rec := make([]string, len(cols))
		for i, col := range cols {
			rec[i] = fmt.Sprintf("%v", row[col])
		}
		if err := w.Write(rec); err != nil {
			return err
		}
	}
	w.Flush()
	return os.WriteFile(path, buf.Bytes(), 0o644)
}

// writeBusinessResult stores a merged detection's output under the business
// results dir, mirroring a single daemon's per-cycle archive shape: result JSON,
// text report, the merged op_metric JSON, plus a cycle.json (ts + trigger time +
// duration + per-category anomaly counts) for the per-business history.
func (c *Center) writeBusinessResult(b *Business, dirRel string, nodeOut *utils.NodeOutput, reportText string, op detector.OpMetric, summary map[string]int, status map[string]string, iterations int, meta roundMeta, startedAt time.Time, durationMs int64, worldSize int) error {
	base := filepath.Join(c.cfg.DataDir, b.Name, dirRel)
	if err := os.MkdirAll(filepath.Join(base, "analysis_result"), 0o755); err != nil {
		return err
	}
	out, _ := json.MarshalIndent(map[string]any{"profiler": nodeOut}, "", "  ")
	if err := os.WriteFile(filepath.Join(base, "straggler_output.json"), out, 0o644); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(base, "analysis_result", "detection_report.log"), []byte(reportText), 0o644); err != nil {
		return err
	}
	opData, _ := json.MarshalIndent(op, "", "  ")
	if err := os.WriteFile(filepath.Join(base, "op_metric.json"), opData, 0o644); err != nil {
		return err
	}
	metaJSON, _ := json.MarshalIndent(map[string]any{
		"ts":                 dirRel,
		"started_at":         startedAt.Format(time.RFC3339),
		"duration_ms":        durationMs,
		"world_size":         worldSize,
		"summary":            summary,
		"category_status":    status,
		"iterations":         iterations,
		"report_timeout_sec": meta.ReportTimeoutSec,
		"reported_daemons":   meta.ReportedDaemons,
		"failed_daemons":     meta.FailedDaemons,
		"missing_daemons":    meta.MissingDaemons,
		"partial":            meta.Partial,
		"report_latency_ms":  meta.ReportLatencyMs,
		"busy_eta_ms":        meta.BusyEtaMs,
		"trigger_reason":     b.lastTriggerReason,
		"failed":             false,
	}, "", "  ")
	return os.WriteFile(filepath.Join(base, "cycle.json"), metaJSON, 0o644)
}

func (c *Center) writeFailedCycle(b *Business, startedAt time.Time, durationMs int64, triggerReason, failureReason string, iterations int, meta roundMeta) {
	dirRel := startedAt.Format("20060102-150405")
	base := filepath.Join(c.cfg.DataDir, b.Name, dirRel)
	if err := os.MkdirAll(base, 0o755); err != nil {
		return
	}
	metaJSON, _ := json.MarshalIndent(map[string]any{
		"ts":                 dirRel,
		"started_at":         startedAt.Format(time.RFC3339),
		"duration_ms":        durationMs,
		"world_size":         0,
		"summary":            nil,
		"iterations":         iterations,
		"report_timeout_sec": meta.ReportTimeoutSec,
		"reported_daemons":   meta.ReportedDaemons,
		"failed_daemons":     meta.FailedDaemons,
		"missing_daemons":    meta.MissingDaemons,
		"partial":            meta.Partial,
		"report_latency_ms":  meta.ReportLatencyMs,
		"busy_eta_ms":        meta.BusyEtaMs,
		"trigger_reason":     triggerReason,
		"failure_reason":     failureReason,
		"failed":             true,
	}, "", "  ")
	_ = os.WriteFile(filepath.Join(base, "cycle.json"), metaJSON, 0o644)
}

// ---------------------------------------------------------------------------
// HTTP helpers
// ---------------------------------------------------------------------------

func (c *Center) selfURL() string {
	return fmt.Sprintf("http://127.0.0.1:%d", c.cfg.Port)
}

func httpGet(url string) (*http.Response, error) {
	client := &http.Client{Timeout: 3 * time.Second}
	return client.Get(url)
}

func getJSON(url, key string, out any) error {
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	if key != "" {
		req.Header.Set("X-Match-Key", key)
	}
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return json.NewDecoder(resp.Body).Decode(out)
}

func doJSON(method, url string, body any, key string) (map[string]any, error) {
	var rdr io.Reader
	if body != nil {
		data, _ := json.Marshal(body)
		rdr = bytes.NewReader(data)
	}
	req, err := http.NewRequest(method, url, rdr)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if key != "" {
		req.Header.Set("X-Match-Key", key)
	}
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	if resp.StatusCode >= 300 {
		return out, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return out, nil
}
