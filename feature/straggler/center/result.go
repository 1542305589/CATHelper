package center

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
)

// cycleInfo is one per-cycle detection record surfaced by the per-business
// history endpoint (ts + trigger time + duration + total rank count +
// per-category anomaly counts).
type cycleInfo struct {
	Ts               string            `json:"ts"`
	StartedAt        string            `json:"started_at"`
	DurationMs       int64             `json:"duration_ms"`
	WorldSize        int               `json:"world_size"`
	Summary          map[string]int    `json:"summary"`
	CategoryStatus   map[string]string `json:"category_status,omitempty"`
	Iterations       int               `json:"iterations,omitempty"`
	ReportTimeoutSec int               `json:"report_timeout_sec,omitempty"`
	ReportedDaemons  int               `json:"reported_daemons,omitempty"`
	FailedDaemons    []string          `json:"failed_daemons,omitempty"`
	MissingDaemons   []string          `json:"missing_daemons,omitempty"`
	Partial          bool              `json:"partial,omitempty"`
	ReportLatencyMs  int64             `json:"report_latency_ms,omitempty"`
	BusyEtaMs        int64             `json:"busy_eta_ms,omitempty"`
	TriggerReason    string            `json:"trigger_reason,omitempty"`
	FailureReason    string            `json:"failure_reason,omitempty"`
	Failed           bool              `json:"failed,omitempty"`
	Running          bool              `json:"running,omitempty"` // in-flight round, prepended to history
}

// latestResultDir returns the newest per-cycle result dir under a business, or
// "" when none exists. Result dirs are named by timestamp (20060102-150405), so
// lexicographic order == chronological order.
func (c *Center) latestResultDir(name string) string {
	dirs := c.resultDirs(name)
	if len(dirs) == 0 {
		return ""
	}
	return filepath.Join(c.cfg.DataDir, name, dirs[0])
}

// resultDirs returns the per-cycle result dir names (timestamps) under a
// business, newest first.
func (c *Center) resultDirs(name string) []string {
	base := filepath.Join(c.cfg.DataDir, name)
	entries, err := os.ReadDir(base)
	if err != nil {
		return nil
	}
	var dirs []string
	for _, e := range entries {
		if e.IsDir() {
			dirs = append(dirs, e.Name())
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(dirs)))
	return dirs
}

// cycleInfos returns each cycle's summary (ts + trigger time + duration +
// counts), newest first.
func (c *Center) cycleInfos(name string) []cycleInfo {
	base := filepath.Join(c.cfg.DataDir, name)
	var out []cycleInfo
	for _, ts := range c.resultDirs(name) {
		ci := cycleInfo{Ts: ts, Summary: map[string]int{}}
		if raw, err := os.ReadFile(filepath.Join(base, ts, "cycle.json")); err == nil {
			var m struct {
				StartedAt        string            `json:"started_at"`
				DurationMs       int64             `json:"duration_ms"`
				WorldSize        int               `json:"world_size"`
				Summary          map[string]int    `json:"summary"`
				CategoryStatus   map[string]string `json:"category_status"`
				Iterations       int               `json:"iterations"`
				ReportTimeoutSec int               `json:"report_timeout_sec"`
				ReportedDaemons  int               `json:"reported_daemons"`
				FailedDaemons    []string          `json:"failed_daemons"`
				MissingDaemons   []string          `json:"missing_daemons"`
				Partial          bool              `json:"partial"`
				ReportLatencyMs  int64             `json:"report_latency_ms"`
				BusyEtaMs        int64             `json:"busy_eta_ms"`
				TriggerReason    string            `json:"trigger_reason"`
				FailureReason    string            `json:"failure_reason"`
				Failed           bool              `json:"failed"`
			}
			if json.Unmarshal(raw, &m) == nil {
				ci.StartedAt = m.StartedAt
				ci.DurationMs = m.DurationMs
				ci.WorldSize = m.WorldSize
				ci.TriggerReason = m.TriggerReason
				ci.FailureReason = m.FailureReason
				ci.Failed = m.Failed
				if m.Summary != nil {
					ci.Summary = m.Summary
				}
				ci.CategoryStatus = m.CategoryStatus
				ci.Iterations = m.Iterations
				ci.ReportTimeoutSec = m.ReportTimeoutSec
				ci.ReportedDaemons = m.ReportedDaemons
				ci.FailedDaemons = m.FailedDaemons
				ci.MissingDaemons = m.MissingDaemons
				ci.Partial = m.Partial
				ci.ReportLatencyMs = m.ReportLatencyMs
				ci.BusyEtaMs = m.BusyEtaMs
			}
		}
		out = append(out, ci)
	}

	// Prepend the in-flight round so the console shows it the moment it starts
	// (otherwise the progress is unreachable until the round completes).
	c.mu.Lock()
	var snap progressSnapshot
	haveProgress := false
	inflightIterations := 0
	inflightTimeout := 0
	if b := c.biz[name]; b != nil {
		inflightIterations = businessIterations(b)
		inflightTimeout = c.reportTimeoutLocked(b, healthyDaemons(b.Daemons))
		if b.progress != nil {
			snap = b.progress.snapshot()
			haveProgress = true
		}
	}
	c.mu.Unlock()
	if haveProgress && snap.InFlight {
		out = append([]cycleInfo{{
			Ts:               snap.StartTs,
			StartedAt:        snap.Started,
			Running:          true,
			Summary:          map[string]int{},
			Iterations:       inflightIterations,
			ReportTimeoutSec: inflightTimeout,
		}}, out...)
	}
	return out
}

// resultDir returns the result dir for a specific timestamp, or the latest when
// ts is empty; "" when absent.
func (c *Center) resultDir(name, ts string) string {
	if ts == "" {
		return c.latestResultDir(name)
	}
	dir := filepath.Join(c.cfg.DataDir, name, ts)
	if info, err := os.Stat(dir); err == nil && info.IsDir() {
		return dir
	}
	return ""
}
