package center

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"

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
	mux.HandleFunc("GET /center/business/{name}/history", c.handleBusinessHistory)
	mux.HandleFunc("GET /center/business/{name}/console", c.handleBusinessConsole)
	mux.HandleFunc("GET /center/business/{name}/report", c.handleBusinessReport)
	mux.HandleFunc("GET /center/business/{name}/result", c.handleBusinessResult)
	mux.HandleFunc("GET /center/business/{name}/op_metric", c.handleBusinessOpMetric)
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
}

// businessStatus is the web-visible view of one business.
type businessStatus struct {
	Name        string         `json:"name"`
	IntervalSec int64          `json:"interval_sec"`
	Daemons     []daemonStatus `json:"daemons"`
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
	default:
		return "disconnected"
	}
}

func (c *Center) statusViewLocked() []businessStatus {
	out := make([]businessStatus, 0, len(c.biz))
	for _, b := range c.biz {
		bs := businessStatus{Name: b.Name, IntervalSec: b.IntervalSec, Daemons: make([]daemonStatus, 0, len(b.Daemons))}
		for _, d := range b.Daemons {
			bs.Daemons = append(bs.Daemons, daemonStatus{IP: d.IP, Port: d.Port, State: daemonState(d), CollectWait: d.collectWait})
		}
		out = append(out, bs)
	}
	return out
}

func (c *Center) handleAddBusiness(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name        string    `json:"name"`
		IntervalSec int64     `json:"interval_sec"`
		Daemons     []*Daemon `json:"daemons"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Name == "" {
		http.Error(w, `invalid body: {"name","interval_sec","daemons":[{"ip","port"}]}`, http.StatusBadRequest)
		return
	}
	if req.IntervalSec <= 0 {
		req.IntervalSec = int64(c.cfg.Interval.Seconds())
	}
	c.mu.Lock()
	if _, exists := c.biz[req.Name]; exists {
		c.mu.Unlock()
		http.Error(w, "business already exists", http.StatusConflict)
		return
	}
	b := &Business{Name: req.Name, IntervalSec: req.IntervalSec, Daemons: req.Daemons}
	if b.Daemons == nil {
		b.Daemons = []*Daemon{}
	}
	c.biz[req.Name] = b
	c.save()
	c.mu.Unlock()

	// Attempt to match the newly added daemons (async; the probe loop will
	// classify any that fail as unmatched/other).
	for _, d := range b.Daemons {
		go c.tryMatch(b, d)
	}
	writeJSON(w, b)
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
	c.mu.Unlock()
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
	c.mu.Unlock()

	go c.tryMatch(b, d)
	writeJSON(w, b)
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

	c.mu.Lock()
	defer c.mu.Unlock()
	b, ok := c.biz[business]
	if !ok {
		http.Error(w, "business not found", http.StatusNotFound)
		return
	}
	for _, d := range b.Daemons {
		if d.Addr() != daemon || d.key == "" || d.key != key {
			continue
		}
		var op detector.OpMetric
		if err := json.NewDecoder(r.Body).Decode(&op); err != nil {
			http.Error(w, "invalid op_metric JSON", http.StatusBadRequest)
			return
		}
		d.lastOpMetric = op
		d.lastReportAt = time.Now()
		writeJSON(w, map[string]string{"status": "ok"})
		return
	}
	http.Error(w, "daemon not matched to this center", http.StatusForbidden)
}

// handleBusinessHistory lists the business's per-cycle records (ts + summary),
// newest first.
func (c *Center) handleBusinessHistory(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{"cycles": c.cycleInfos(r.PathValue("name"))})
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

// tryMatch matches a daemon in a goroutine (best-effort on add).
func (c *Center) tryMatch(b *Business, d *Daemon) {
	c.mu.Lock()
	err := c.matchDaemonLocked(b, d)
	c.mu.Unlock()
	if err != nil {
		c.logf("business %s daemon %s match: %v", b.Name, d.Addr(), err)
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(v)
}