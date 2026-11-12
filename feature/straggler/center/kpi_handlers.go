package center

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

const maxKPIReportBytes = 16 << 20

// handleKPIIngest receives one daemon's NPU KPI samples and stores them in the
// center's SQLite history. Auth mirrors handleOpMetric: the business must exist,
// the daemon must belong to it, and X-Match-Key must match the daemon's key.
func (c *Center) handleKPIIngest(w http.ResponseWriter, r *http.Request) {
	business := r.PathValue("business")
	daemon := r.PathValue("daemon")
	key := r.Header.Get("X-Match-Key")

	if c.kpi == nil {
		http.Error(w, "kpi store disabled", http.StatusServiceUnavailable)
		return
	}
	if !c.kpiDaemonAuthorized(business, daemon, key) {
		http.Error(w, "daemon not matched to this center", http.StatusForbidden)
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, maxKPIReportBytes+1))
	if err != nil {
		http.Error(w, "read body: "+err.Error(), http.StatusBadRequest)
		return
	}
	if len(body) > maxKPIReportBytes {
		http.Error(w, "kpi report too large", http.StatusRequestEntityTooLarge)
		return
	}
	var report kpiReport
	if err := json.Unmarshal(body, &report); err != nil {
		http.Error(w, "invalid kpi report: "+err.Error(), http.StatusBadRequest)
		return
	}
	if len(report.Samples) == 0 {
		http.Error(w, "kpi report has no samples", http.StatusBadRequest)
		return
	}
	if len(report.Samples) > 10000 {
		http.Error(w, "too many kpi samples", http.StatusRequestEntityTooLarge)
		return
	}
	node := kpiNodeLabel(report.Node, daemon)
	rows, err := c.kpi.Insert(business, daemon, node, report.Samples)
	if err != nil {
		c.logf("kpi ingest: business=%q daemon=%q: %v", business, daemon, err)
		http.Error(w, "store kpi: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if rows == 0 {
		http.Error(w, "no valid kpi rows", http.StatusBadRequest)
		return
	}
	writeJSON(w, map[string]any{"status": "ok", "samples": len(report.Samples), "rows": rows})
}

// handleBusinessKPIDevices returns all node/device pairs from the latest KPI data,
// ignoring the allowlist, so the console can render selectable cards.
func (c *Center) handleBusinessKPIDevices(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !c.businessExists(name) {
		http.Error(w, "business not found", http.StatusNotFound)
		return
	}
	if c.kpi == nil {
		http.Error(w, "kpi store disabled", http.StatusServiceUnavailable)
		return
	}
	nodes, err := c.kpi.Devices(name)
	if err != nil {
		http.Error(w, "query kpi devices: "+err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"business": name, "nodes": nodes})
}

// handleBusinessKPIFilterGet returns the stored card allowlists for a business.
func (c *Center) handleBusinessKPIFilterGet(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !c.businessExists(name) {
		http.Error(w, "business not found", http.StatusNotFound)
		return
	}
	if c.kpi == nil {
		http.Error(w, "kpi store disabled", http.StatusServiceUnavailable)
		return
	}
	filters, err := c.kpi.Filters(name)
	if err != nil {
		http.Error(w, "query kpi filter: "+err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"business": name, "filters": filters})
}

// handleBusinessKPIFilterSet replaces one node's card allowlist. An empty devices
// list removes the filter (all cards on that node are allowed).
func (c *Center) handleBusinessKPIFilterSet(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !c.businessExists(name) {
		http.Error(w, "business not found", http.StatusNotFound)
		return
	}
	if c.kpi == nil {
		http.Error(w, "kpi store disabled", http.StatusServiceUnavailable)
		return
	}
	var req struct {
		Node      string `json:"node"`
		Devices   []int  `json:"devices"`
		FilterAll bool   `json:"filter_all"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		http.Error(w, `invalid body: {"node","devices":[0,1,...]}`, http.StatusBadRequest)
		return
	}
	req.Node = strings.TrimSpace(req.Node)
	if req.Node == "" {
		http.Error(w, "node is required", http.StatusBadRequest)
		return
	}
	for _, device := range req.Devices {
		if device < 0 {
			http.Error(w, "devices must be non-negative", http.StatusBadRequest)
			return
		}
	}
	if err := c.kpi.SetFilter(name, req.Node, req.Devices, req.FilterAll); err != nil {
		http.Error(w, "store kpi filter: "+err.Error(), http.StatusInternalServerError)
		return
	}
	filters, err := c.kpi.Filters(name)
	if err != nil {
		http.Error(w, "query kpi filter: "+err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"business": name, "filters": filters})
}

// kpiNodeLabel annotates a daemon-reported hostname with its daemon identity so
// two daemons that report the same hostname remain distinct nodes, e.g.
// "master(51.62.10.87:15424)". An empty hostname falls back to the daemon.
func kpiNodeLabel(node, daemon string) string {
	node = strings.TrimSpace(node)
	daemon = strings.TrimSpace(daemon)
	if node == "" {
		return daemon
	}
	if daemon == "" {
		return node
	}
	suffix := "(" + daemon + ")"
	if strings.HasSuffix(node, suffix) {
		return node
	}
	return node + suffix
}

func (c *Center) kpiDaemonAuthorized(business, daemon, key string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	b := c.biz[business]
	if b == nil {
		return false
	}
	for _, d := range b.Daemons {
		if d.Addr() == daemon {
			return d.Key != "" && d.Key == key
		}
	}
	return false
}

func (c *Center) businessExists(name string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, ok := c.biz[name]
	return ok
}

// handleBusinessConsoleKPI serves the business console's KPI detection tab. It is a
// console-only view of recent detection runs and anomalies, not a general KPI query API.
func (c *Center) handleBusinessConsoleKPI(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !c.businessExists(name) {
		http.Error(w, "business not found", http.StatusNotFound)
		return
	}
	if c.kpi == nil {
		http.Error(w, "kpi store disabled", http.StatusServiceUnavailable)
		return
	}
	limit := 200
	if raw := strings.TrimSpace(r.URL.Query().Get("limit")); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 && n <= 1000 {
			limit = n
		}
	}
	runsTo := time.Now().Unix()
	runsFrom := runsTo - 24*3600
	if raw := strings.TrimSpace(r.URL.Query().Get("from")); raw != "" {
		v, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || v <= 0 {
			http.Error(w, "invalid from", http.StatusBadRequest)
			return
		}
		runsFrom = v
	}
	if raw := strings.TrimSpace(r.URL.Query().Get("to")); raw != "" {
		v, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || v <= 0 {
			http.Error(w, "invalid to", http.StatusBadRequest)
			return
		}
		runsTo = v
	}
	if runsTo <= runsFrom {
		http.Error(w, "to must be greater than from", http.StatusBadRequest)
		return
	}
	runs, err := c.kpi.DetectRunsRange(name, runsFrom, runsTo, 5000)
	if err != nil {
		http.Error(w, "query kpi runs: "+err.Error(), http.StatusInternalServerError)
		return
	}
	var anomalies []kpiAnomaly
	if raw := strings.TrimSpace(r.URL.Query().Get("ts")); raw != "" {
		ts, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || ts <= 0 {
			http.Error(w, "invalid ts", http.StatusBadRequest)
			return
		}
		anomalies, err = c.kpi.AnomaliesAt(name, ts)
	} else {
		anomalies, err = c.kpi.RecentAnomalies(name, limit)
	}
	if err != nil {
		http.Error(w, "query kpi anomalies: "+err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"business": name, "runs": runs, "anomalies": anomalies})
}

// handleBusinessKPIInterval updates one business's KPI detection period. This is
// independent of the business's profiler trigger interval.
func (c *Center) handleBusinessKPIInterval(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	var req struct {
		IntervalSec int64 `json:"interval_sec"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		http.Error(w, `invalid body: {"interval_sec":10}`, http.StatusBadRequest)
		return
	}
	if req.IntervalSec < 5 || req.IntervalSec > 86400 {
		http.Error(w, "interval_sec must be in [5, 86400]", http.StatusBadRequest)
		return
	}
	c.mu.Lock()
	b := c.biz[name]
	if b == nil {
		c.mu.Unlock()
		http.Error(w, "business not found", http.StatusNotFound)
		return
	}
	b.KPIDetectIntervalSec = req.IntervalSec
	b.nextKPIDetect = time.Now().Add(time.Duration(req.IntervalSec) * time.Second)
	c.save()
	c.mu.Unlock()
	writeJSON(w, map[string]any{"business": name, "kpi_interval_sec": req.IntervalSec})
}

// handleBusinessKPINSigma updates one business's per-metric KPI n-sigma thresholds.
func (c *Center) handleBusinessKPINSigma(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	var req KPINSigma
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		http.Error(w, `invalid body: {"power_time","power_space","util_time","util_space","freq_time","freq_space"}`, http.StatusBadRequest)
		return
	}
	req = req.Normalized()
	for _, v := range []float64{req.PowerTime, req.PowerSpace, req.UtilTime, req.UtilSpace, req.FreqTime, req.FreqSpace} {
		if v <= 0 || v > 20 {
			http.Error(w, "n-sigma must be in (0, 20]", http.StatusBadRequest)
			return
		}
	}
	c.mu.Lock()
	b := c.biz[name]
	if b == nil {
		c.mu.Unlock()
		http.Error(w, "business not found", http.StatusNotFound)
		return
	}
	b.KPINSigma = req
	c.save()
	c.mu.Unlock()
	writeJSON(w, map[string]any{"business": name, "kpi_nsigma": req})
}

type kpiSeriesPoint struct {
	Ts    int64    `json:"ts"`
	Power *float64 `json:"power,omitempty"`
	Util  *float64 `json:"aicore_util,omitempty"`
	Freq  *float64 `json:"aicore_freq,omitempty"`
}

type kpiSeries struct {
	Node   string           `json:"node"`
	Device int              `json:"device"`
	Points []kpiSeriesPoint `json:"points"`
}

// handleBusinessKPISeries returns 1-minute downsampled KPI history for the
// console chart. It is console-only and applies the business device allowlist.
func (c *Center) handleBusinessKPISeries(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !c.businessExists(name) {
		http.Error(w, "business not found", http.StatusNotFound)
		return
	}
	if c.kpi == nil {
		http.Error(w, "kpi store disabled", http.StatusServiceUnavailable)
		return
	}
	now := time.Now().Unix()
	end, err := parseInt64Query(r, "ts", now)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	hours, err := parseInt64Query(r, "hours", 24)
	if err != nil || hours <= 0 || hours > 168 {
		http.Error(w, "hours must be in [1, 168]", http.StatusBadRequest)
		return
	}
	step, err := parseInt64Query(r, "step", 5)
	if err != nil || step <= 0 || step > 300 {
		http.Error(w, "step must be in [1, 300]", http.StatusBadRequest)
		return
	}
	from := end - hours*3600
	rows, err := c.kpi.Series(name, from, end, step)
	if err != nil {
		http.Error(w, "query kpi series: "+err.Error(), http.StatusInternalServerError)
		return
	}
	filters, err := c.kpi.Filters(name)
	if err != nil {
		http.Error(w, "query kpi filter: "+err.Error(), http.StatusInternalServerError)
		return
	}
	byNode := make(map[string]kpiFilter, len(filters))
	for _, f := range filters {
		byNode[f.Node] = f
	}
	seriesMap := make(map[string]*kpiSeries)
	for _, row := range rows {
		filter := byNode[row.Node]
		if filter.FilterAll {
			continue
		}
		if len(filter.Devices) > 0 {
			allowed := false
			for _, device := range filter.Devices {
				if device == row.Device {
					allowed = true
					break
				}
			}
			if !allowed {
				continue
			}
		}
		key := row.Node + "|" + strconv.Itoa(row.Device)
		s := seriesMap[key]
		if s == nil {
			s = &kpiSeries{Node: row.Node, Device: row.Device}
			seriesMap[key] = s
		}
		s.Points = append(s.Points, kpiSeriesPoint{
			Ts: row.Ts, Power: row.Power, Util: row.Util, Freq: row.Freq,
		})
	}
	out := make([]*kpiSeries, 0, len(seriesMap))
	for _, s := range seriesMap {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Node != out[j].Node {
			return out[i].Node < out[j].Node
		}
		return out[i].Device < out[j].Device
	})
	writeJSON(w, map[string]any{"business": name, "from": from, "to": end, "series": out})
}

func parseInt64Query(r *http.Request, key string, def int64) (int64, error) {
	raw := strings.TrimSpace(r.URL.Query().Get(key))
	if raw == "" {
		return def, nil
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid %s %q", key, raw)
	}
	return n, nil
}

// handleBusinessKPIReference updates the probability that an anomalous KPI point is
// still admitted into the reference window.
func (c *Center) handleBusinessKPIReference(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	var req struct {
		AdmitRatio float64 `json:"admit_ratio"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		http.Error(w, `invalid body: {"admit_ratio":0.1}`, http.StatusBadRequest)
		return
	}
	if req.AdmitRatio <= 0 || req.AdmitRatio > 1 {
		http.Error(w, "admit_ratio must be in (0, 1]", http.StatusBadRequest)
		return
	}
	c.mu.Lock()
	b := c.biz[name]
	if b == nil {
		c.mu.Unlock()
		http.Error(w, "business not found", http.StatusNotFound)
		return
	}
	b.KPIReferenceAdmitRatio = req.AdmitRatio
	c.save()
	c.mu.Unlock()
	writeJSON(w, map[string]any{"business": name, "kpi_reference_admit_ratio": req.AdmitRatio})
}
