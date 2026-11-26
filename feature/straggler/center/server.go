package center

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Computing-Availability-Tools/CATHelper/feature/straggler/config"
	"github.com/Computing-Availability-Tools/CATHelper/feature/straggler/profiling/detector"
)

// httpServer builds the center's HTTP mux.
func (c *Center) httpServer() *http.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", c.handleConsole)
	mux.HandleFunc("GET /center/healthz", c.handleHealthz)
	mux.HandleFunc("GET /center/businesses", c.handleListBusinesses)
	mux.HandleFunc("POST /center/business", c.handleAddBusiness)
	mux.HandleFunc("DELETE /center/business/{name}", c.handleRemoveBusiness)
	mux.HandleFunc("POST /center/business/{name}/daemon", c.handleAddDaemon)
	mux.HandleFunc("DELETE /center/business/{name}/daemon", c.handleRemoveDaemon)
	mux.HandleFunc("POST /center/business/{name}/daemon/match", c.handleMatchDaemon)
	mux.HandleFunc("POST /center/business/{name}/daemon/unmatch", c.handleUnmatchDaemon)
	mux.HandleFunc("POST /center/op_metric/{business}/{daemon}", c.handleOpMetric)
	mux.HandleFunc("POST /center/kpi/{business}/{daemon}", c.handleKPIIngest)
	mux.HandleFunc("GET /center/business/{name}/kpi/devices", c.handleBusinessKPIDevices)
	mux.HandleFunc("GET /center/business/{name}/console/kpi", c.handleBusinessConsoleKPI)
	mux.HandleFunc("GET /center/business/{name}/console/kpi/series", c.handleBusinessKPISeries)
	mux.HandleFunc("POST /center/business/{name}/kpi_interval", c.handleBusinessKPIInterval)
	mux.HandleFunc("POST /center/business/{name}/kpi_nsigma", c.handleBusinessKPINSigma)
	mux.HandleFunc("POST /center/business/{name}/kpi_reference", c.handleBusinessKPIReference)
	mux.HandleFunc("GET /center/business/{name}/kpi/filter", c.handleBusinessKPIFilterGet)
	mux.HandleFunc("POST /center/business/{name}/kpi/filter", c.handleBusinessKPIFilterSet)
	mux.HandleFunc("GET /center/business/{name}/history", c.handleBusinessHistory)
	mux.HandleFunc("GET /center/business/{name}/progress", c.handleBusinessProgress)
	mux.HandleFunc("GET /center/business/{name}/console", c.handleBusinessConsole)
	mux.HandleFunc("GET /center/chart.umd.min.js", c.handleChartJS)
	mux.HandleFunc("GET /center/business/{name}/report", c.handleBusinessReport)
	mux.HandleFunc("GET /center/business/{name}/result", c.handleBusinessResult)
	mux.HandleFunc("GET /center/business/{name}/op_metric", c.handleBusinessOpMetric)
	mux.HandleFunc("POST /center/business/{name}/trigger", c.handleBusinessTrigger)
	mux.HandleFunc("POST /center/business/{name}/pause", c.handleBusinessPause)
	mux.HandleFunc("POST /center/business/{name}/start", c.handleBusinessStart)
	mux.HandleFunc("POST /center/business/{name}/interval", c.handleBusinessInterval)
	mux.HandleFunc("POST /center/business/{name}/iterations", c.handleBusinessIterations)
	mux.HandleFunc("POST /center/business/{name}/thresholds", c.handleBusinessThresholds)
	mux.HandleFunc("POST /center/business/{name}/vllm", c.handleSetVLLMMetrics)
	mux.HandleFunc("DELETE /center/business/{name}/vllm", c.handleUnsetVLLMMetrics)
	mux.HandleFunc("GET /center/business/{name}/metrics", c.handleBusinessMetrics)
	return &http.Server{Handler: mux}
}

func (c *Center) handleHealthz(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

func (c *Center) handleListBusinesses(w http.ResponseWriter, r *http.Request) {
	c.mu.Lock()
	defer c.mu.Unlock()
	writeJSON(w, map[string]any{"businesses": c.statusViewLocked()})
}

// daemonStatus is the web-visible view of one daemon (runtime match/health
// state included).
type daemonStatus struct {
	IP          string `json:"ip"`
	Port        int    `json:"port"`
	State       string `json:"state"` // healthy / unmatched / other / disconnected
	CollectWait int64  `json:"collect_wait"`
	Iterations  int    `json:"iterations,omitempty"` // daemon's saved self-run value (0 = unknown / old build)
	ReportEtaMs int64  `json:"report_eta_ms,omitempty"`
}

// businessStatus is the web-visible view of one business.
type businessStatus struct {
	Name                   string            `json:"name"`
	IntervalSec            int64             `json:"interval_sec"`
	Iterations             int               `json:"iterations"` // center trigger iterations for this business
	ReportEtaMs            int64             `json:"report_eta_ms"`
	ReportTimeoutSec       int               `json:"report_timeout_sec"`
	MinIntervalSec         int64             `json:"min_interval_sec"`
	IntervalWarning        string            `json:"interval_warning,omitempty"`
	KPIDetectIntervalSec   int64             `json:"kpi_interval_sec"`
	KPINSigma              KPINSigma         `json:"kpi_nsigma"`
	KPIReferenceAdmitRatio float64           `json:"kpi_reference_admit_ratio"`
	Paused                 bool              `json:"paused"`
	CyclesTotal            int               `json:"cycles_total"`
	CyclesFailed           int               `json:"cycles_failed"`
	NextTrigger            string            `json:"next_trigger,omitempty"`
	CollectWait            int64             `json:"collect_wait"`           // max across the business's daemons
	VLLMMetrics            string            `json:"vllm_metrics,omitempty"` // vllm /metrics endpoint URL
	Thresholds             config.Thresholds `json:"thresholds"`             // merged-detection thresholds
	DefaultThresholds      config.Thresholds `json:"default_thresholds"`     // factory defaults, for "restore defaults"
	Daemons                []daemonStatus    `json:"daemons"`
}

func daemonState(d *Daemon) string {
	switch d.matchState {
	case "healthy":
		return "healthy"
	case "unmatched":
		return "unmatched"
	case "other":
		return "other"
	case "report_failed":
		return "report_failed"
	case "reported_failed":
		return "reported_failed"
	default:
		return "disconnected"
	}
}

func (c *Center) statusViewLocked() []businessStatus {
	out := make([]businessStatus, 0, len(c.biz))
	for _, b := range c.biz {
		out = append(out, c.businessStatusOfLocked(b))
	}
	// Stable ordering: c.biz is a map whose iteration order is random, so sort
	// by name to keep the console from reshuffling businesses on every poll.
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// businessStatusOfLocked builds the web-safe status view for one business
// (never exposing the daemons' match keys).
func (c *Center) businessStatusOfLocked(b *Business) businessStatus {
	healthy := healthyDaemons(b.Daemons)
	timeout := c.reportTimeoutLocked(b, healthy)
	minInterval := int64(timeout + businessPostProcessSec)
	warning := ""
	if b.IntervalSec > 0 && b.IntervalSec < minInterval {
		warning = fmt.Sprintf("interval_sec %d below current minimum %d (report_timeout + 120s)", b.IntervalSec, minInterval)
	}
	bs := businessStatus{
		Name:                   b.Name,
		IntervalSec:            b.IntervalSec,
		Iterations:             businessIterations(b),
		ReportEtaMs:            c.maxReportEtaLocked(healthy),
		ReportTimeoutSec:       timeout,
		MinIntervalSec:         minInterval,
		IntervalWarning:        warning,
		KPIDetectIntervalSec:   b.KPIDetectIntervalSec,
		KPINSigma:              b.KPINSigma.Normalized(),
		KPIReferenceAdmitRatio: b.NormalizedKPIReferenceAdmitRatio(),
		Paused:                 b.Paused,
		CyclesTotal:            b.CyclesTotal,
		CyclesFailed:           b.CyclesFailed,
		VLLMMetrics:            b.VLLMMetrics,
		Thresholds:             b.Thresholds,
		DefaultThresholds:      config.DefaultThresholds(),
		Daemons:                make([]daemonStatus, 0, len(b.Daemons)),
	}
	if !b.NextTrigger.IsZero() {
		bs.NextTrigger = b.NextTrigger.Format(time.RFC3339)
	}
	for _, d := range b.Daemons {
		bs.Daemons = append(bs.Daemons, daemonStatus{
			IP: d.IP, Port: d.Port, State: daemonState(d), CollectWait: d.collectWait,
			Iterations: d.reportedIterations, ReportEtaMs: c.daemonReportEtaLocked(d),
		})
		if d.collectWait > bs.CollectWait {
			bs.CollectWait = d.collectWait
		}
	}
	return bs
}

func (c *Center) handleAddBusiness(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name                 string    `json:"name"`
		IntervalSec          int64     `json:"interval_sec"`
		KPIDetectIntervalSec int64     `json:"kpi_interval_sec"`
		Daemons              []*Daemon `json:"daemons"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Name == "" {
		http.Error(w, `invalid body: {"name","interval_sec","kpi_interval_sec","daemons":[{"ip","port"}]}`, http.StatusBadRequest)
		return
	}
	if req.IntervalSec <= 0 {
		req.IntervalSec = int64(c.cfg.Interval.Seconds())
	}
	newBusinessMinInterval := int64(defaultReportTimeoutSec + businessPostProcessSec)
	if req.IntervalSec < newBusinessMinInterval {
		http.Error(w, fmt.Sprintf("interval_sec %d below minimum %d (default report_timeout %d + 120s)",
			req.IntervalSec, newBusinessMinInterval, defaultReportTimeoutSec), http.StatusBadRequest)
		return
	}
	if req.KPIDetectIntervalSec <= 0 {
		req.KPIDetectIntervalSec = int64(c.cfg.KPIDetectInterval.Seconds())
	}
	if req.KPIDetectIntervalSec < 5 || req.KPIDetectIntervalSec > 86400 {
		http.Error(w, "kpi_interval_sec must be in [5, 86400]", http.StatusBadRequest)
		return
	}
	c.mu.Lock()
	if _, exists := c.biz[req.Name]; exists {
		c.mu.Unlock()
		http.Error(w, "business already exists", http.StatusConflict)
		return
	}
	b := &Business{Name: req.Name, IntervalSec: req.IntervalSec, KPIDetectIntervalSec: req.KPIDetectIntervalSec, Iterations: defaultBusinessIterations, Daemons: req.Daemons}
	if b.Daemons == nil {
		b.Daemons = []*Daemon{}
	}
	if b.Thresholds == (config.Thresholds{}) {
		b.Thresholds = c.cfg.Thresholds
	} else {
		b.Thresholds = b.Thresholds.Normalized()
	}
	b.progress = newProgressLog()
	c.biz[req.Name] = b
	c.save()
	view := c.businessStatusOfLocked(b)
	c.mu.Unlock()

	// Attempt to match the newly added daemons (async; the probe loop will
	// classify any that fail as unmatched/other).
	for _, d := range b.Daemons {
		go c.tryMatch(b, d)
	}
	writeJSON(w, view)
}

func (c *Center) handleRemoveBusiness(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	c.mu.Lock()
	b, ok := c.biz[name]
	if !ok {
		c.mu.Unlock()
		http.Error(w, "business not found", http.StatusNotFound)
		return
	}
	for _, d := range b.Daemons {
		c.unmatchDaemonLocked(d)
	}
	delete(c.biz, name)
	c.save()
	c.metrics.clear(name)
	c.mu.Unlock()
	if c.kpi != nil {
		if err := c.kpi.DeleteBusiness(name); err != nil {
			c.logf("remove business %s kpi data: %v", name, err)
		}
	}
	if err := os.RemoveAll(filepath.Join(c.cfg.DataDir, name)); err != nil {
		c.logf("remove business %s result dir: %v", name, err)
	}
	writeJSON(w, map[string]string{"status": "removed"})
}

func (c *Center) handleAddDaemon(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	var req struct {
		IP   string `json:"ip"`
		Port int    `json:"port"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.IP == "" || req.Port <= 0 {
		http.Error(w, `invalid body: {"ip","port"}`, http.StatusBadRequest)
		return
	}
	c.mu.Lock()
	b, ok := c.biz[name]
	if !ok {
		c.mu.Unlock()
		http.Error(w, "business not found", http.StatusNotFound)
		return
	}
	if c.daemonIndex(b, req.IP, req.Port) >= 0 {
		c.mu.Unlock()
		http.Error(w, "daemon already in business", http.StatusConflict)
		return
	}
	d := &Daemon{IP: req.IP, Port: req.Port}
	b.Daemons = append(b.Daemons, d)
	c.save()
	view := c.businessStatusOfLocked(b)
	c.mu.Unlock()

	go c.tryMatch(b, d)
	writeJSON(w, view)
}

func (c *Center) handleRemoveDaemon(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	ip := r.URL.Query().Get("ip")
	port, _ := strconv.Atoi(r.URL.Query().Get("port"))
	c.mu.Lock()
	b, ok := c.biz[name]
	if !ok {
		c.mu.Unlock()
		http.Error(w, "business not found", http.StatusNotFound)
		return
	}
	idx := c.daemonIndex(b, ip, port)
	if idx < 0 {
		c.mu.Unlock()
		http.Error(w, "daemon not in business", http.StatusNotFound)
		return
	}
	c.unmatchDaemonLocked(b.Daemons[idx])
	b.Daemons = append(b.Daemons[:idx], b.Daemons[idx+1:]...)
	c.save()
	c.mu.Unlock()
	writeJSON(w, map[string]string{"status": "removed"})
}

// handleMatchDaemon re-matches a daemon (user's "reconnect" action for an
// unmatched / other-center daemon).
func (c *Center) handleMatchDaemon(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	var req struct {
		IP   string `json:"ip"`
		Port int    `json:"port"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.IP == "" || req.Port <= 0 {
		http.Error(w, `invalid body: {"ip","port"}`, http.StatusBadRequest)
		return
	}
	c.mu.Lock()
	b, ok := c.biz[name]
	if !ok {
		c.mu.Unlock()
		http.Error(w, "business not found", http.StatusNotFound)
		return
	}
	idx := c.daemonIndex(b, req.IP, req.Port)
	if idx < 0 {
		c.mu.Unlock()
		http.Error(w, "daemon not in business", http.StatusNotFound)
		return
	}
	err := c.matchDaemonLocked(b, b.Daemons[idx])
	c.mu.Unlock()
	if err != nil {
		http.Error(w, "match failed: "+err.Error(), http.StatusBadGateway)
		return
	}
	writeJSON(w, map[string]string{"status": "matched"})
}

// handleUnmatchDaemon manually unmatches a daemon (back to the daemon's own
// "unmatched" state).
func (c *Center) handleUnmatchDaemon(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	var req struct {
		IP   string `json:"ip"`
		Port int    `json:"port"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.IP == "" || req.Port <= 0 {
		http.Error(w, `invalid body: {"ip","port"}`, http.StatusBadRequest)
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	b, ok := c.biz[name]
	if !ok {
		http.Error(w, "business not found", http.StatusNotFound)
		return
	}
	idx := c.daemonIndex(b, req.IP, req.Port)
	if idx < 0 {
		http.Error(w, "daemon not in business", http.StatusNotFound)
		return
	}
	c.unmatchDaemonLocked(b.Daemons[idx])
	writeJSON(w, map[string]string{"status": "unmatched"})
}

// handleOpMetric receives a daemon's reported op_metric JSON (key-authenticated)
// and stores it for the current trigger round.
func (c *Center) handleOpMetric(w http.ResponseWriter, r *http.Request) {
	business := r.PathValue("business")
	daemon := r.PathValue("daemon")
	key := r.Header.Get("X-Match-Key")
	c.logf("op_metric: business=%q daemon=%q keyLen=%d", business, daemon, len(key))

	c.mu.Lock()
	defer c.mu.Unlock()
	b, ok := c.biz[business]
	if !ok {
		http.Error(w, "business not found", http.StatusNotFound)
		return
	}
	for _, d := range b.Daemons {
		if d.Addr() != daemon || d.Key == "" || d.Key != key {
			continue
		}
		body, _ := io.ReadAll(r.Body)
		// New envelope: {"round_token","status","reason","stage","ranks"}.
		// Also accept the legacy {cycle,dir,ranks} envelope and the bare rank map.
		var env struct {
			RoundToken string            `json:"round_token"`
			Status     string            `json:"status"`
			Reason     string            `json:"reason"`
			Stage      string            `json:"stage"`
			BusyEtaMs  int64             `json:"busy_eta_ms"`
			Ranks      detector.OpMetric `json:"ranks"`
		}
		var op detector.OpMetric
		var roundToken, status, reason, stage string
		if err := json.Unmarshal(body, &env); err == nil && (env.Ranks != nil || env.Status != "" || env.RoundToken != "") {
			roundToken, status, reason, stage, op = env.RoundToken, env.Status, env.Reason, env.Stage, env.Ranks
		} else if err := json.Unmarshal(body, &op); err != nil || op == nil {
			c.logf("op_metric decode error: %v", err)
			http.Error(w, "invalid op_metric JSON", http.StatusBadRequest)
			return
		}
		if status == "" {
			status = "ok" // legacy daemon
		}
		switch status {
		case "ok", "failed", "busy", "paused":
		default:
			http.Error(w, "invalid report status: "+status, http.StatusBadRequest)
			return
		}
		if status == "ok" && op == nil {
			http.Error(w, "ok report requires ranks", http.StatusBadRequest)
			return
		}
		// A report after the round closed, or carrying another round's token,
		// must not affect this round (or a later one).
		if !b.triggering {
			c.logf("late op_metric ignored: business=%q daemon=%q status=%s token=%q", business, daemon, status, roundToken)
			writeJSON(w, map[string]string{"status": "ignored"})
			return
		}
		if roundToken != "" && b.pendingToken != "" && roundToken != b.pendingToken {
			c.logf("stale op_metric ignored: business=%q daemon=%q token=%q pending=%q", business, daemon, roundToken, b.pendingToken)
			writeJSON(w, map[string]string{"status": "ignored"})
			return
		}
		d.lastReportAt = time.Now()
		d.lastRoundToken = roundToken
		d.lastReportStatus = status
		d.lastReportReason = reason
		d.lastReportStage = stage
		d.lastBusyEtaMs = 0
		if status == "busy" {
			d.lastBusyEtaMs = env.BusyEtaMs
			c.logf("op_metric busy: business=%q daemon=%q busy_eta_ms=%d", business, daemon, env.BusyEtaMs)
		}
		if status == "ok" {
			d.lastOpMetric = op
		} else {
			// Never let a later round reuse a cached payload from a failed report.
			d.lastOpMetric = nil
		}
		writeJSON(w, map[string]string{"status": "ok"})
		return
	}
	c.logf("op_metric not matched: daemon %q (biz daemons=%d)", daemon, len(b.Daemons))
	http.Error(w, "daemon not matched to this center", http.StatusForbidden)
}

// handleBusinessHistory lists the business's per-cycle records (ts + summary),
// newest first.
func (c *Center) handleBusinessHistory(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{"cycles": c.cycleInfos(r.PathValue("name"))})
}

// handleBusinessProgress returns a round's stage log: ?ts= selects a specific
// round (from its persisted progress.json, or the live in-memory log when it is
// the current one); omitted = the current/last round.
func (c *Center) handleBusinessProgress(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	ts := r.URL.Query().Get("ts")
	c.mu.Lock()
	b := c.biz[name]
	var p *progressLog
	if b != nil {
		p = b.progress
	}
	c.mu.Unlock()
	if b == nil {
		http.Error(w, "business not found", http.StatusNotFound)
		return
	}

	if ts == "" {
		if p != nil {
			writeJSON(w, p.snapshot())
			return
		}
		writeJSON(w, progressSnapshot{})
		return
	}

	if p != nil {
		if snap := p.snapshot(); snap.InFlight && snap.StartTs == ts {
			writeJSON(w, snap)
			return
		}
	}
	if raw, err := os.ReadFile(filepath.Join(c.cfg.DataDir, name, ts, "progress.json")); err == nil {
		var snap progressSnapshot
		if json.Unmarshal(raw, &snap) == nil {
			writeJSON(w, snap)
			return
		}
	}
	writeJSON(w, progressSnapshot{})
}

// handleBusinessReport serves the business's merged detection report (text/plain)
// for a given cycle (?ts=), latest when omitted.
func (c *Center) handleBusinessReport(w http.ResponseWriter, r *http.Request) {
	dir := c.resultDir(r.PathValue("name"), r.URL.Query().Get("ts"))
	if dir == "" {
		http.Error(w, "no result yet", http.StatusNotFound)
		return
	}
	path := filepath.Join(dir, "analysis_result", "detection_report.log")
	raw, err := os.ReadFile(path)
	if err != nil {
		http.Error(w, "no report yet", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write(raw)
}

// handleBusinessResult serves the business's merged result JSON for a given
// cycle (?ts=), latest when omitted.
func (c *Center) handleBusinessResult(w http.ResponseWriter, r *http.Request) {
	dir := c.resultDir(r.PathValue("name"), r.URL.Query().Get("ts"))
	if dir == "" {
		http.Error(w, "no result yet", http.StatusNotFound)
		return
	}
	http.ServeFile(w, r, filepath.Join(dir, "straggler_output.json"))
}

// handleBusinessOpMetric serves the business's merged op_metric JSON for a
// given cycle (?ts=), latest when omitted.
func (c *Center) handleBusinessOpMetric(w http.ResponseWriter, r *http.Request) {
	dir := c.resultDir(r.PathValue("name"), r.URL.Query().Get("ts"))
	if dir == "" {
		http.Error(w, "no result yet", http.StatusNotFound)
		return
	}
	http.ServeFile(w, r, filepath.Join(dir, "op_metric.json"))
}

// handleBusinessTrigger immediately triggers one business's detection round.
func (c *Center) handleBusinessTrigger(w http.ResponseWriter, r *http.Request) {
	c.mu.Lock()
	b := c.biz[r.PathValue("name")]
	if b == nil {
		c.mu.Unlock()
		http.Error(w, "business not found", http.StatusNotFound)
		return
	}
	if b.Paused {
		c.mu.Unlock()
		http.Error(w, "business is paused", http.StatusConflict)
		return
	}
	if b.triggering {
		b.pendingRound = true
		b.pendingReason = mergeTriggerReason(b.pendingReason, "manual")
		b.queueIfBusyNext = true
		if b.NextTrigger.IsZero() || b.NextTrigger.After(time.Now()) {
			b.NextTrigger = time.Now()
		}
		c.logf("business %s: round in flight; queued manual round", b.Name)
		c.save()
		c.mu.Unlock()
		w.WriteHeader(http.StatusAccepted)
		writeJSON(w, map[string]string{"status": "queued"})
		return
	}
	ready := c.businessReadyLocked(b)
	c.mu.Unlock()
	if !ready {
		http.Error(w, "not all daemons healthy", http.StatusConflict)
		return
	}
	go c.triggerBusinessMode(b, true, "manual")
	writeJSON(w, map[string]string{"status": "triggered"})
}

// handleBusinessPause pauses a business's scheduling.
func (c *Center) handleBusinessPause(w http.ResponseWriter, r *http.Request) {
	c.mu.Lock()
	defer c.mu.Unlock()
	b := c.biz[r.PathValue("name")]
	if b == nil {
		http.Error(w, "business not found", http.StatusNotFound)
		return
	}
	b.Paused = true
	b.pendingRound = false
	b.pendingReason = ""
	b.queueIfBusyNext = false
	c.save()
	writeJSON(w, map[string]string{"status": "paused"})
}

// handleBusinessStart resumes a business's scheduling.
func (c *Center) handleBusinessStart(w http.ResponseWriter, r *http.Request) {
	c.mu.Lock()
	defer c.mu.Unlock()
	b := c.biz[r.PathValue("name")]
	if b == nil {
		http.Error(w, "business not found", http.StatusNotFound)
		return
	}
	b.Paused = false
	b.pendingRound = false
	b.pendingReason = ""
	b.queueIfBusyNext = false
	b.NextTrigger = time.Now().Add(c.businessInterval(b))
	c.save()
	writeJSON(w, map[string]string{"status": "started"})
}

// handleBusinessInterval updates a business's trigger period.
func (c *Center) handleBusinessInterval(w http.ResponseWriter, r *http.Request) {
	var req struct {
		IntervalSec int64 `json:"interval_sec"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.IntervalSec <= 0 {
		http.Error(w, `invalid body: {"interval_sec": 600}`, http.StatusBadRequest)
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	b := c.biz[r.PathValue("name")]
	if b == nil {
		http.Error(w, "business not found", http.StatusNotFound)
		return
	}
	minInterval := int64(c.reportTimeoutLocked(b, healthyDaemons(b.Daemons)) + businessPostProcessSec)
	if req.IntervalSec < minInterval {
		http.Error(w, fmt.Sprintf("interval_sec %d below minimum %d (report_timeout + 120s)", req.IntervalSec, minInterval), http.StatusBadRequest)
		return
	}
	b.IntervalSec = req.IntervalSec
	b.NextTrigger = time.Now().Add(time.Duration(req.IntervalSec) * time.Second)
	c.save()
	writeJSON(w, map[string]any{"interval_sec": req.IntervalSec})
}

// handleBusinessIterations updates the business-level iteration count used by
// EVERY center trigger (manual, timer, KPI-triggered). It is carried by the
// /daemon/trigger request and does not modify any daemon's saved self-run value.
func (c *Center) handleBusinessIterations(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Iterations int `json:"iterations"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil ||
		req.Iterations < minBusinessIterations || req.Iterations > maxBusinessIterations {
		http.Error(w, fmt.Sprintf(`invalid body: {"iterations": 3} (range [%d, %d])`, minBusinessIterations, maxBusinessIterations), http.StatusBadRequest)
		return
	}
	c.mu.Lock()
	b := c.biz[r.PathValue("name")]
	if b == nil {
		c.mu.Unlock()
		http.Error(w, "business not found", http.StatusNotFound)
		return
	}
	b.Iterations = req.Iterations
	c.save()
	c.mu.Unlock()

	writeJSON(w, map[string]any{"iterations": req.Iterations})
}

// handleBusinessThresholds updates a business's merged-detection thresholds.
// Only affects future detection rounds; past results are untouched.
func (c *Center) handleBusinessThresholds(w http.ResponseWriter, r *http.Request) {
	var req config.Thresholds
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, `invalid body: {"cal","cpu","comm_threshold","comm_min_count","comm_count_floor"}`, http.StatusBadRequest)
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	b := c.biz[r.PathValue("name")]
	if b == nil {
		http.Error(w, "business not found", http.StatusNotFound)
		return
	}
	b.Thresholds = req.Normalized()
	c.save()
	writeJSON(w, b.Thresholds)
}

// handleSetVLLMMetrics attaches a vllm /metrics endpoint URL to a business.
// Replaces any existing URL and resets the collected series.
func (c *Center) handleSetVLLMMetrics(w http.ResponseWriter, r *http.Request) {
	var req struct {
		URL string `json:"url"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, `invalid body: {"url"}`, http.StatusBadRequest)
		return
	}
	req.URL = strings.TrimSpace(req.URL)
	if !strings.HasPrefix(req.URL, "http://") && !strings.HasPrefix(req.URL, "https://") {
		http.Error(w, `url 需以 http:// 或 https:// 开头`, http.StatusBadRequest)
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	b := c.biz[r.PathValue("name")]
	if b == nil {
		http.Error(w, "business not found", http.StatusNotFound)
		return
	}
	b.VLLMMetrics = req.URL
	c.save()
	c.metrics.clear(b.Name)
	// Scrape immediately so the chart starts populating without waiting for the
	// next 20s tick (this first scrape only seeds the cumulative baseline).
	go func() {
		c.metrics.scrape(&http.Client{Timeout: metricsTimeout}, b.Name, req.URL)
	}()
	writeJSON(w, map[string]string{"vllm_metrics": req.URL})
}

// handleUnsetVLLMMetrics removes a business's vllm /metrics endpoint.
func (c *Center) handleUnsetVLLMMetrics(w http.ResponseWriter, r *http.Request) {
	c.mu.Lock()
	defer c.mu.Unlock()
	b := c.biz[r.PathValue("name")]
	if b == nil {
		http.Error(w, "business not found", http.StatusNotFound)
		return
	}
	b.VLLMMetrics = ""
	c.save()
	c.metrics.clear(b.Name)
	writeJSON(w, map[string]string{"status": "removed"})
}

// handleBusinessMetrics returns the collected TTFT/TPOT time series (per
// engine) for the business's vllm endpoint.
func (c *Center) handleBusinessMetrics(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	c.mu.Lock()
	_, ok := c.biz[name]
	c.mu.Unlock()
	if !ok {
		http.Error(w, "business not found", http.StatusNotFound)
		return
	}

	fromRaw := strings.TrimSpace(r.URL.Query().Get("from"))
	toRaw := strings.TrimSpace(r.URL.Query().Get("to"))
	if fromRaw == "" && toRaw == "" {
		writeJSON(w, c.metrics.snapshot(name))
		return
	}
	if fromRaw == "" || toRaw == "" {
		http.Error(w, "from and to are required together", http.StatusBadRequest)
		return
	}
	from, err := strconv.ParseInt(fromRaw, 10, 64)
	if err != nil || from <= 0 {
		http.Error(w, "invalid from", http.StatusBadRequest)
		return
	}
	to, err := strconv.ParseInt(toRaw, 10, 64)
	if err != nil || to <= from {
		http.Error(w, "invalid to", http.StatusBadRequest)
		return
	}
	if to-from > int64((7*24*time.Hour)/time.Second) {
		http.Error(w, "range must be at most 7 days", http.StatusBadRequest)
		return
	}
	if c.kpi == nil {
		http.Error(w, "kpi store disabled", http.StatusServiceUnavailable)
		return
	}
	engines, err := c.kpi.VLLMMetricsRange(name, from, to)
	if err != nil {
		http.Error(w, "query vllm metrics: "+err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{
		"business": name,
		"from":     from,
		"to":       to,
		"source":   "sqlite",
		"engines":  engines,
	})
}

// tryMatch matches a daemon in a goroutine (best-effort on add). The HTTP
// call runs outside c.mu so a slow daemon cannot block the center's web UI.
func (c *Center) tryMatch(b *Business, d *Daemon) {
	c.mu.Lock()
	if !c.daemonPresentLocked(b, d) {
		c.mu.Unlock()
		return
	}
	key := randomKey()
	d.Key = key
	c.save()
	c.mu.Unlock()

	body := map[string]any{
		"center_addr":  c.selfURL(),
		"key":          key,
		"business":     b.Name,
		"daemon":       d.Addr(),
		"interval_sec": int64(c.businessInterval(b).Seconds()),
	}
	resp, err := doJSON(http.MethodPost, d.BaseURL()+"/daemon/match", body, "")

	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.daemonPresentLocked(b, d) || d.Key != key {
		return
	}
	if err != nil {
		d.Key = ""
		c.save()
		c.logf("business %s daemon %s match: %v", b.Name, d.Addr(), err)
		return
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
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(v)
}
