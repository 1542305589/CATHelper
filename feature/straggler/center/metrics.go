package center

import (
	"io"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	metricsInterval  = 20 * time.Second // scrape cadence == delta window
	metricsMaxPoints = 900              // ~5h of points at 20s cadence
	metricsTimeout   = 10 * time.Second
)

// metricsLoop scrapes every business's vllm /metrics endpoint on a fixed
// cadence and records delta-average TTFT/TPOT per engine.
func (c *Center) metricsLoop() {
	ticker := time.NewTicker(metricsInterval)
	defer ticker.Stop()
	client := &http.Client{Timeout: metricsTimeout}
	for range ticker.C {
		c.scrapeAll(client)
	}
}

// scrapeAll snapshots the current business → URL mapping under lock, then
// fetches each endpoint outside the lock (a slow endpoint must not block the
// rest of the center).
func (c *Center) scrapeAll(client *http.Client) {
	c.mu.Lock()
	type job struct{ name, url string }
	jobs := make([]job, 0, len(c.biz))
	for _, b := range c.biz {
		if b.VLLMMetrics != "" {
			jobs = append(jobs, job{b.Name, b.VLLMMetrics})
		}
	}
	c.mu.Unlock()
	for _, j := range jobs {
		c.metrics.scrape(client, j.name, j.url)
	}
}

// metricPoint is one sampled datapoint for one engine.
type metricPoint struct {
	Ts      int64   `json:"ts"`      // unix seconds
	TTFT    float64 `json:"ttft"`    // seconds, delta-average (carry-forward when idle)
	TPOT    float64 `json:"tpot"`    // seconds, delta-average (carry-forward when idle)
	Reqs    float64 `json:"reqs"`    // requests completed in this window (0 => idle)
	Running float64 `json:"running"` // vllm:num_requests_running gauge
	Waiting float64 `json:"waiting"` // vllm:num_requests_waiting gauge
}

// metricCum is the cumulative histogram sum/count for one engine, used to
// compute window deltas between two scrapes.
type metricCum struct {
	ttftSum, ttftCount, tpotSum, tpotCount float64
	running, waiting                       float64
	hasRunning, hasWaiting                 bool
}

// engineMetrics is the per-engine series plus the last cumulative sample.
type engineMetrics struct {
	points []metricPoint
	last   metricCum
	have   bool // first scrape only seeds `last`, no delta point yet
}

// metricsStore keeps in-memory time series per business, keyed by engine.
type metricsStore struct {
	mu     sync.Mutex
	series map[string]map[string]*engineMetrics // business → engine → series
	url    map[string]string                    // business → last seen metrics URL
	lastAt map[string]int64                     // business → last scrape unix seconds
	err    map[string]string                    // business → last scrape error
	store  *kpiStore
	logf   func(format string, args ...any)
}

func newMetricsStore() *metricsStore {
	return &metricsStore{
		series: map[string]map[string]*engineMetrics{},
		url:    map[string]string{},
		lastAt: map[string]int64{},
		err:    map[string]string{},
		logf:   func(format string, args ...any) {},
	}
}

// scrape fetches one business's /metrics endpoint and records delta-average
// TTFT/TPOT per engine. On any failure it records the error and leaves the
// previous series untouched.
func (m *metricsStore) scrape(client *http.Client, business, url string) {
	resp, err := client.Get(url)
	if err != nil {
		m.fail(business, url, err.Error())
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		m.fail(business, url, "HTTP "+resp.Status)
		return
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		m.fail(business, url, err.Error())
		return
	}
	m.record(business, url, parseVLLMMetrics(string(body)), time.Now().Unix())
}

func (m *metricsStore) record(business, url string, sums map[string]metricCum, now int64) {
	m.mu.Lock()
	m.url[business] = url
	m.lastAt[business] = now
	delete(m.err, business)

	eng, ok := m.series[business]
	if !ok {
		eng = map[string]*engineMetrics{}
		m.series[business] = eng
	}
	persist := map[string]metricPoint{}
	for name, c := range sums {
		e := eng[name]
		if e == nil {
			e = &engineMetrics{}
			eng[name] = e
		}
		if !e.have {
			e.last = c
			e.have = true
			continue
		}
		p := metricPoint{Ts: now, Running: c.running, Waiting: c.waiting}
		valid := false
		if c.hasRunning || c.hasWaiting {
			valid = true
		}

		dCount := c.ttftCount - e.last.ttftCount
		if dCount > 0 {
			p.TTFT = (c.ttftSum - e.last.ttftSum) / dCount
			p.Reqs = dCount
			valid = true
		} else if n := len(e.points); n > 0 {
			p.TTFT = e.points[n-1].TTFT
			valid = true
		}

		dCount = c.tpotCount - e.last.tpotCount
		if dCount > 0 {
			p.TPOT = (c.tpotSum - e.last.tpotSum) / dCount
			valid = true
		} else if n := len(e.points); n > 0 {
			p.TPOT = e.points[n-1].TPOT
		}

		e.last = c
		if valid {
			e.points = append(e.points, p)
			if len(e.points) > metricsMaxPoints {
				e.points = e.points[len(e.points)-metricsMaxPoints:]
			}
			persist[name] = p
		}
	}
	m.mu.Unlock()

	if m.store != nil && len(persist) > 0 {
		if err := m.store.InsertVLLMMetrics(business, persist); err != nil {
			m.logf("vllm metrics persist: %v", err)
		}
	}
}

func (m *metricsStore) fail(business, url, msg string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.url[business] = url
	m.err[business] = msg
}

// clear drops all metrics for a business (called when its vllm URL is removed
// or the business is deleted).
func (m *metricsStore) clear(business string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.series, business)
	delete(m.url, business)
	delete(m.lastAt, business)
	delete(m.err, business)
}

// snapshot returns the web-facing view of one business's metrics.
func (m *metricsStore) snapshot(business string) map[string]any {
	m.mu.Lock()
	defer m.mu.Unlock()
	engines := map[string][]metricPoint{}
	if eng, ok := m.series[business]; ok {
		for name, e := range eng {
			if len(e.points) == 0 {
				continue
			}
			engines[name] = e.points
		}
	}
	return map[string]any{
		"url":        m.url[business],
		"last_at":    m.lastAt[business],
		"last_error": m.err[business],
		"engines":    engines,
	}
}

// loadSnapshot is the /metrics-derived load state used by KPI detection.
// It is observe-only for now: it is stored with each detection run but does not
// change global_shift, profiler triggering, or reference-window behavior.
type loadSnapshot struct {
	Reqs5m         float64
	RunningP90     float64
	RunningMax     float64
	BaselineReqs5m float64
	LoadRatio      float64
	Points         int
	BaselinePoints int
	Fresh          bool
}

// LoadSnapshot aggregates the current 5-minute request window and a 5-hour
// baseline from the in-memory /metrics series.
func (m *metricsStore) LoadSnapshot(business string, now int64) loadSnapshot {
	m.mu.Lock()
	defer m.mu.Unlock()
	eng, ok := m.series[business]
	if !ok {
		return loadSnapshot{}
	}
	const curWindow = int64(5 * 60)
	const baseWindow = int64(5 * 3600)
	since := now - curWindow
	baseSince := now - baseWindow
	var reqs float64
	var running []float64
	baseBuckets := make(map[int64]float64)
	points := 0
	basePoints := 0
	for _, e := range eng {
		for _, p := range e.points {
			if p.Ts < baseSince || p.Ts > now {
				continue
			}
			if p.Ts >= since {
				reqs += p.Reqs
				running = append(running, p.Running)
				points++
			} else {
				baseBuckets[p.Ts/300] += p.Reqs
				basePoints++
			}
		}
	}
	if points == 0 {
		return loadSnapshot{}
	}
	sort.Float64s(running)
	p90 := 0.0
	if len(running) > 0 {
		idx := int(math.Ceil(0.9*float64(len(running)))) - 1
		if idx < 0 {
			idx = 0
		}
		if idx >= len(running) {
			idx = len(running) - 1
		}
		p90 = running[idx]
	}
	baseVals := make([]float64, 0, len(baseBuckets))
	for _, v := range baseBuckets {
		baseVals = append(baseVals, v)
	}
	sort.Float64s(baseVals)
	baseline := 0.0
	if len(baseVals) > 0 {
		baseline = baseVals[len(baseVals)/2]
	}
	ratio := 0.0
	if baseline > 0 {
		ratio = reqs / baseline
	}
	fresh := m.lastAt[business] > 0 && now-m.lastAt[business] <= 60
	return loadSnapshot{
		Reqs5m: reqs, RunningP90: p90, RunningMax: maxFloat(running),
		BaselineReqs5m: baseline, LoadRatio: ratio,
		Points: points, BaselinePoints: basePoints, Fresh: fresh,
	}
}

func maxFloat(v []float64) float64 {
	if len(v) == 0 {
		return 0
	}
	return v[len(v)-1]
}

// classifyLoad maps /metrics load signals to idle/low/active/transition/unknown.
func classifyLoad(s loadSnapshot) string {
	if !s.Fresh || s.Points == 0 {
		return "unknown"
	}
	reqs, base, running := s.Reqs5m, s.BaselineReqs5m, s.RunningP90
	if running == 0 && (base <= 0 || reqs < math.Max(1, 0.05*base)) {
		return "idle"
	}
	if (base > 0 && reqs >= math.Max(3, 0.30*base)) || running >= 2 {
		return "active"
	}
	if running <= 1 || (base > 0 && reqs < math.Max(2, 0.10*base)) {
		return "low"
	}
	return "transition"
}

// parseVLLMMetrics extracts cumulative sum/count for TTFT and TPOT per engine
// from a Prometheus text-format body. Engines without an `engine` label are
// grouped under "default".
func parseVLLMMetrics(text string) map[string]metricCum {
	out := map[string]metricCum{}
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		name, labels, value, ok := parsePromLine(line)
		if !ok {
			continue
		}
		engine := labelValue(labels, "engine")
		if engine == "" {
			engine = "default"
		}
		c := out[engine]
		switch name {
		case "vllm:time_to_first_token_seconds_sum":
			c.ttftSum = value
		case "vllm:time_to_first_token_seconds_count":
			c.ttftCount = value
		// TPOT (inter-token latency): newer vllm renamed the metric from
		// vllm:time_per_output_token_seconds to vllm:inter_token_latency_seconds;
		// accept both so old and new servers chart identically.
		case "vllm:inter_token_latency_seconds_sum", "vllm:time_per_output_token_seconds_sum":
			c.tpotSum = value
		case "vllm:inter_token_latency_seconds_count", "vllm:time_per_output_token_seconds_count":
			c.tpotCount = value
		case "vllm:num_requests_running":
			c.running = value
			c.hasRunning = true
		case "vllm:num_requests_waiting":
			c.waiting = value
			c.hasWaiting = true
		default:
			continue
		}
		out[engine] = c
	}
	return out
}

// parsePromLine splits "name{labels} value[ timestamp]" into its parts.
func parsePromLine(line string) (name, labels string, value float64, ok bool) {
	open := strings.IndexByte(line, '{')
	if open < 0 {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			return "", "", 0, false
		}
		v, err := strconv.ParseFloat(fields[1], 64)
		if err != nil {
			return "", "", 0, false
		}
		return fields[0], "", v, true
	}
	name = line[:open]
	closeIdx := strings.IndexByte(line, '}')
	if closeIdx < 0 {
		return "", "", 0, false
	}
	labels = line[open+1 : closeIdx]
	fields := strings.Fields(strings.TrimSpace(line[closeIdx+1:]))
	if len(fields) < 1 {
		return "", "", 0, false
	}
	v, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return "", "", 0, false
	}
	return name, labels, v, true
}

// labelValue extracts one label's value from a raw label string like
// `engine="0",model_name="x"`.
func labelValue(labels, key string) string {
	prefix := key + `="`
	idx := strings.Index(labels, prefix)
	if idx < 0 {
		return ""
	}
	start := idx + len(prefix)
	end := strings.IndexByte(labels[start:], '"')
	if end < 0 {
		return ""
	}
	return labels[start : start+end]
}
