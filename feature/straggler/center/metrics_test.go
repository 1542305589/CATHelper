package center

import (
	"math"
	"testing"
)

// parseVLLMMetrics must extract TTFT and TPOT sum/count per engine from a
// Prometheus text body, accepting BOTH the modern TPOT name
// (vllm:inter_token_latency_seconds) and the legacy name
// (vllm:time_per_output_token_seconds).
func TestParseVLLMMetrics(t *testing.T) {
	text := `# HELP vllm:time_to_first_token_seconds Histogram of time to first token in seconds.
# TYPE vllm:time_to_first_token_seconds histogram
vllm:time_to_first_token_seconds_sum{engine="0",model_name="x"} 1.5
vllm:time_to_first_token_seconds_count{engine="0",model_name="x"} 10
vllm:inter_token_latency_seconds_sum{engine="0",model_name="x"} 0.8
vllm:inter_token_latency_seconds_count{engine="0",model_name="x"} 40
vllm:time_to_first_token_seconds_sum{engine="1",model_name="x"} 2.0
vllm:time_to_first_token_seconds_count{engine="1",model_name="x"} 5
vllm:time_per_output_token_seconds_sum{engine="1",model_name="x"} 0.6
vllm:time_per_output_token_seconds_count{engine="1",model_name="x"} 30
`
	got := parseVLLMMetrics(text)

	if _, ok := got["0"]; !ok {
		t.Fatalf("engine 0 missing, got %v", got)
	}
	c0 := got["0"]
	if c0.ttftSum != 1.5 || c0.ttftCount != 10 || c0.tpotSum != 0.8 || c0.tpotCount != 40 {
		t.Errorf("engine 0 = %+v, want ttftSum=1.5 ttftCount=10 tpotSum=0.8 tpotCount=40", c0)
	}

	c1 := got["1"]
	if c1.ttftSum != 2.0 || c1.ttftCount != 5 || c1.tpotSum != 0.6 || c1.tpotCount != 30 {
		t.Errorf("engine 1 = %+v, want ttftSum=2.0 ttftCount=5 tpotSum=0.6 tpotCount=30", c1)
	}
}

// Metrics without an engine label are grouped under "default".
func TestParseVLLMMetricsDefaultEngine(t *testing.T) {
	text := `vllm:time_to_first_token_seconds_sum{model_name="x"} 3.0
vllm:inter_token_latency_seconds_count{model_name="x"} 7
`
	got := parseVLLMMetrics(text)
	c := got["default"]
	if c.ttftSum != 3.0 || c.tpotCount != 7 {
		t.Errorf("default = %+v, want ttftSum=3.0 tpotCount=7", c)
	}
}

// A single scrape seeds the baseline; the second scrape's delta average becomes
// the first data point.
func TestMetricsRecordDelta(t *testing.T) {
	m := newMetricsStore()
	m.record("biz", "http://x/metrics", map[string]metricCum{
		"0": {ttftSum: 10, ttftCount: 10, tpotSum: 5, tpotCount: 100},
	}, 1000)
	snap := m.snapshot("biz")
	if len(snap["engines"].(map[string][]metricPoint)) != 0 {
		t.Fatalf("first scrape should only seed baseline, got %v", snap["engines"])
	}

	m.record("biz", "http://x/metrics", map[string]metricCum{
		"0": {ttftSum: 13, ttftCount: 20, tpotSum: 6.2, tpotCount: 200},
	}, 1020)
	snap = m.snapshot("biz")
	pts := snap["engines"].(map[string][]metricPoint)["0"]
	if len(pts) != 1 {
		t.Fatalf("want 1 point after second scrape, got %d", len(pts))
	}
	p := pts[0]
	// TTFT delta: (13-10)/(20-10) = 0.3; TPOT delta: (6.2-5)/(200-100) = 0.012.
	if math.Abs(p.TTFT-0.3) > 1e-9 || math.Abs(p.TPOT-0.012) > 1e-9 || p.Reqs != 10 {
		t.Errorf("point = %+v, want TTFT=0.3 TPOT=0.012 Reqs=10", p)
	}
}
