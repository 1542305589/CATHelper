package center

import (
	"io"
	"net/http"
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
	Ts   int64   `json:"ts"`   // unix seconds
	TTFT float64 `json:"ttft"` // seconds, delta-average (carry-forward when idle)
	TPOT float64 `json:"tpot"` // seconds, delta-average (carry-forward when idle)
	Reqs float64 `json:"reqs"` // requests completed in this window (0 => idle)
}

// metricCum is the cumulative histogram sum/count for one engine, used to
// compute window deltas between two scrapes.
type metricCum struct {
	ttftSum, ttftCount, tpotSum, tpotCount float64
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
}

func newMetricsStore() *metricsStore {
	return &metricsStore{
		series: map[string]map[string]*engineMetrics{},
		url:    map[string]string{},
		lastAt: map[string]int64{},
		err:    map[string]string{},
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
	defer m.mu.Unlock()
	m.url[business] = url
	m.lastAt[business] = now
	delete(m.err, business)

	eng, ok := m.series[business]
	if !ok {
		eng = map[string]*engineMetrics{}
		m.series[business] = eng
	}
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
		p := metricPoint{Ts: now}
		valid := false

		dCount := c.ttftCount - e.last.ttftCount
		if dCount > 0 {
			p.TTFT = (c.ttftSum - e.last.ttftSum) / dCount
			p.Reqs = dCount
			valid = true
		} else if n := len(e.points); n > 0 {
			p.TTFT = e.points[n-1].TTFT // carry-forward when idle
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
