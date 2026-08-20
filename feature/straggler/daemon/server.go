package daemon

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
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
		HistorySize:  d.cfg.HistorySize,
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

// handleResultsLatest serves the most recent cycle's combined result JSON
// (the query API data source). Falls back to the newest dump on disk so a
// daemon restart does not lose the last result.
func (d *Daemon) handleResultsLatest(w http.ResponseWriter, r *http.Request) {
	if c := d.st.latest(); c != nil && c.JSONPath != "" && fileExists(c.JSONPath) {
		http.ServeFile(w, r, c.JSONPath)
		return
	}
	metas := d.listMetaFiles()
	if len(metas) > 0 {
		p := filepath.Join(metas[0].DumpDir, "straggler_output.json")
		if fileExists(p) {
			http.ServeFile(w, r, p)
			return
		}
	}
	http.Error(w, "no result yet", http.StatusNotFound)
}

// handleResultsHistory lists cycle summaries, newest first, scanning each dump
// directory's daemon_meta.json (survives restart). ?limit=N caps the list.
func (d *Daemon) handleResultsHistory(w http.ResponseWriter, r *http.Request) {
	limit := 10
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	metas := d.listMetaFiles()
	if len(metas) > limit {
		metas = metas[:limit]
	}
	resp := historyResponse{Cycles: make([]*cycleSummary, 0, len(metas))}
	for _, m := range metas {
		resp.Cycles = append(resp.Cycles, toCycleSummary(m))
	}
	writeJSON(w, resp)
}

// handleResultsByID serves one cycle's combined result JSON, by id (session
// cache first, then disk meta files).
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
	for _, m := range d.listMetaFiles() {
		if m.ID == id {
			p := filepath.Join(m.DumpDir, "straggler_output.json")
			if fileExists(p) {
				http.ServeFile(w, r, p)
				return
			}
		}
	}
	http.NotFound(w, r)
}

// handleReportLatest serves the most recent cycle's text report (text/plain).
func (d *Daemon) handleReportLatest(w http.ResponseWriter, r *http.Request) {
	if c := d.st.latest(); c != nil && c.Report != "" {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = io.WriteString(w, c.Report)
		return
	}
	metas := d.listMetaFiles()
	if len(metas) > 0 {
		p := filepath.Join(metas[0].DumpDir, "analysis_result", "detection_report.log")
		if fileExists(p) {
			http.ServeFile(w, r, p)
			return
		}
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

// ---------------------------------------------------------------------------
// Disk metadata helpers (daemon_meta.json in each dump directory)
// ---------------------------------------------------------------------------

// listMetaFiles parses daemon_meta.json from every dump directory under
// ProfilerDir, newest first. This is the restart-surviving history source.
func (d *Daemon) listMetaFiles() []*CycleResult {
	entries, err := os.ReadDir(d.cfg.ProfilerDir)
	if err != nil {
		return nil
	}
	var metas []*CycleResult
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		raw, rerr := os.ReadFile(filepath.Join(d.cfg.ProfilerDir, e.Name(), "daemon_meta.json"))
		if rerr != nil {
			continue
		}
		var cr CycleResult
		if json.Unmarshal(raw, &cr) != nil {
			continue
		}
		metas = append(metas, &cr)
	}
	sort.Slice(metas, func(i, j int) bool {
		return metas[i].StartedAt.After(metas[j].StartedAt)
	})
	return metas
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
