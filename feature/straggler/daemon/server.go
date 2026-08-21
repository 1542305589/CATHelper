package daemon

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
)

// httpServer builds the net/http mux (standard library only). Paths carry no
// /api/v1 prefix: /status + /straggler/* for queries, /daemon/* for control.
func (d *Daemon) httpServer() *http.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "ok")
	})
	mux.HandleFunc("GET /status", d.handleStatus)
	mux.HandleFunc("GET /straggler/results/latest", d.handleResultsLatest)
	mux.HandleFunc("GET /straggler/results/history", d.handleResultsHistory)
	mux.HandleFunc("GET /straggler/results/{id}", d.handleResultsByID)
	mux.HandleFunc("GET /straggler/report/latest", d.handleReportLatest)
	mux.HandleFunc("POST /daemon/start", d.handleDaemonStart)
	mux.HandleFunc("POST /daemon/pause", d.handleDaemonPause)
	mux.HandleFunc("POST /daemon/interval", d.handleDaemonInterval)
	mux.HandleFunc("POST /daemon/trigger", d.handleDaemonTrigger)
	return &http.Server{Addr: fmt.Sprintf(":%d", d.cfg.Port), Handler: mux}
}

// handleStatus reports the daemon state, the two data dirs, and session stats.
func (d *Daemon) handleStatus(w http.ResponseWriter, r *http.Request) {
	d.mu.Lock()
	state := d.state
	interval := d.interval
	nextRun := d.nextRun
	d.mu.Unlock()
	total, failed := d.st.counts()

	resp := statusResponse{
		State:        state,
		IntervalSec:  int64(interval.Seconds()),
		ProfilerDir:  d.cfg.ProfilerDir,
		KpiDir:       d.cfg.KpiDir,
		CyclesTotal:  total,
		CyclesFailed: failed,
	}
	if c := d.st.latest(); c != nil {
		resp.LastCycle = toCycleSummary(c)
	}
	if state == "running" && !nextRun.IsZero() {
		t := nextRun
		resp.NextRunAt = &t
	}
	writeJSON(w, resp)
}

// handleResultsLatest serves the most recent cycle's combined result JSON,
// from this session only (no disk history).
func (d *Daemon) handleResultsLatest(w http.ResponseWriter, r *http.Request) {
	if c := d.st.latest(); c != nil && c.JSONPath != "" && fileExists(c.JSONPath) {
		http.ServeFile(w, r, c.JSONPath)
		return
	}
	http.Error(w, "no result yet", http.StatusNotFound)
}

// handleResultsHistory lists this session's cycle summaries, newest first.
// The full session history is returned by default; ?limit=N caps the list.
func (d *Daemon) handleResultsHistory(w http.ResponseWriter, r *http.Request) {
	limit := 0 // 0 = no cap: all of this session's cycles
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	cycles := d.st.list()
	if limit > 0 && len(cycles) > limit {
		cycles = cycles[:limit]
	}
	resp := historyResponse{Cycles: make([]*cycleSummary, 0, len(cycles))}
	for _, c := range cycles {
		resp.Cycles = append(resp.Cycles, toCycleSummary(c))
	}
	writeJSON(w, resp)
}

// handleResultsByID serves one cycle's combined result JSON by id, from this
// session only.
func (d *Daemon) handleResultsByID(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}
	if c := d.st.get(id); c != nil && c.JSONPath != "" && fileExists(c.JSONPath) {
		http.ServeFile(w, r, c.JSONPath)
		return
	}
	http.NotFound(w, r)
}

// handleReportLatest serves the most recent cycle's text report (text/plain),
// from this session only.
func (d *Daemon) handleReportLatest(w http.ResponseWriter, r *http.Request) {
	if c := d.st.latest(); c != nil && c.Report != "" {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = io.WriteString(w, c.Report)
		return
	}
	http.Error(w, "no report yet", http.StatusNotFound)
}

func (d *Daemon) handleDaemonStart(w http.ResponseWriter, r *http.Request) {
	d.Start()
	writeJSON(w, map[string]string{"state": "running"})
}

func (d *Daemon) handleDaemonPause(w http.ResponseWriter, r *http.Request) {
	d.Pause()
	writeJSON(w, map[string]string{"state": "paused"})
}

func (d *Daemon) handleDaemonInterval(w http.ResponseWriter, r *http.Request) {
	var req struct {
		IntervalSec int64 `json:"interval_sec"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid body: {\"interval_sec\": 300}", http.StatusBadRequest)
		return
	}
	if err := d.SetInterval(req.IntervalSec); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, struct {
		IntervalSec int64 `json:"interval_sec"`
	}{req.IntervalSec})
}

func (d *Daemon) handleDaemonTrigger(w http.ResponseWriter, r *http.Request) {
	if err := d.Trigger(); err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	writeJSON(w, map[string]string{"status": "triggered"})
}

func toCycleSummary(c *CycleResult) *cycleSummary {
	if c == nil {
		return nil
	}
	return &cycleSummary{
		ID:         c.ID,
		StartedAt:  c.StartedAt,
		FinishedAt: c.FinishedAt,
		DurationMs: c.DurationMs,
		DBs:        c.DBs,
		DumpDir:    c.DumpDir,
		Summary:    c.Summary,
		KPIStatus:  c.KPIStatus,
		Error:      c.Error,
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(v)
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}
