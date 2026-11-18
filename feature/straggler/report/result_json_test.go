package report

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Computing-Availability-Tools/CATHelper/feature/straggler/config"
)

// The result JSON must always contain all six categories (empty arrays when
// clean), use the Python flat item shape, and render comm keys as domain[ranks].
func TestWriteDetectionResultSchema(t *testing.T) {
	dir := t.TempDir()
	result := config.DegradationData{
		"cal":          {"2": 4.0},
		"kernel_aivec": {},
		"memcpy_async": {},
		"comm":         {"0,1": 2.0},
		"pp_comm":      {},
		"cpu":          {"nodeB": 10.0},
	}
	parallels := map[string][][]int{"tp": {{0, 1}, {2, 3}}}

	if err := WriteDetectionResult(dir, result, parallels); err != nil {
		t.Fatalf("WriteDetectionResult: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "straggler_detection_result.json"))
	if err != nil {
		t.Fatalf("read result: %v", err)
	}
	var got map[string][]map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, raw)
	}
	for _, cat := range []string{"KERNEL_AICORE", "kernel_aivec", "memcpy_async", "comm", "pp_comm", "cpu"} {
		if _, ok := got[cat]; !ok {
			t.Fatalf("category %q missing from %v", cat, got)
		}
	}
	if len(got["kernel_aivec"]) != 0 || len(got["memcpy_async"]) != 0 || len(got["pp_comm"]) != 0 {
		t.Fatalf("empty categories must be [], got %v", got)
	}
	if len(got["KERNEL_AICORE"]) != 1 {
		t.Fatalf("KERNEL_AICORE = %v, want one item", got["KERNEL_AICORE"])
	}
	item := got["KERNEL_AICORE"][0]
	if item["display_key"] != "2" || item["is_abnormal"] != true {
		t.Fatalf("item = %v", item)
	}
	if v, _ := item["metric_value"].(float64); v != 4.0 {
		t.Fatalf("metric_value = %v, want 4", item["metric_value"])
	}
	comm := got["comm"][0]
	if comm["display_key"] != "tp[0, 1]" {
		t.Fatalf("comm display_key = %v, want tp[0, 1]", comm["display_key"])
	}
}

// The stdout table uses 类别 | 异常点位 | 劣化指数 | 劣化阈值.
func TestRenderSummaryTable(t *testing.T) {
	result := config.DegradationData{
		"cal":  {"2": 4.0},
		"comm": {"0,1": 2.0},
	}
	parallels := map[string][][]int{"tp": {{0, 1}, {2, 3}}}
	th := config.Thresholds{Cal: 1.3, CPU: 2.5, CommThreshold: 1.3}
	out := RenderSummaryTable(result, parallels, th)
	for _, want := range []string{"KERNEL_AICORE", "tp[0, 1]", "2:4", "1.3x"} {
		if !strings.Contains(out, want) {
			t.Fatalf("table missing %q:\n%s", want, out)
		}
	}
}

// pp_comm is rendered as a share of the session (Python: val*100 with %g),
// not as a multiple.
func TestRenderSummaryTablePPPercent(t *testing.T) {
	result := config.DegradationData{"pp_comm": {"0,1": 0.07}}
	out := RenderSummaryTable(result, nil, config.Thresholds{Cal: 1.3, CPU: 2.5, CommThreshold: 1.3})
	for _, want := range []string{"[0, 1]", "7%", ">5%"} {
		if !strings.Contains(out, want) {
			t.Fatalf("table missing %q:\n%s", want, out)
		}
	}
}

// A clean result renders an explicit "no anomaly" conclusion.
func TestRenderConclusionNoAnomaly(t *testing.T) {
	out := RenderConclusion(config.DegradationData{}, nil, config.Thresholds{Cal: 1.3, CPU: 2.5, CommThreshold: 1.3})
	if !strings.Contains(out, "未检出异常") {
		t.Fatalf("conclusion = %q, want 未检出异常", out)
	}
}

// A compute hit suppresses PP detection; the conclusion must say PP was not
// evaluated instead of leaving an empty pp_comm reading as "PP is healthy".
func TestRenderConclusionPPSkipped(t *testing.T) {
	result := config.DegradationData{"cal": {"3": 4.0}}
	parallels := map[string][][]int{"tp": {{0, 1}, {2, 3}}}
	out := RenderConclusion(result, parallels, config.Thresholds{Cal: 1.3, CPU: 2.5, CommThreshold: 1.3})
	if !strings.Contains(out, "pp_comm）未评估") {
		t.Fatalf("conclusion missing PP-skipped note:\n%s", out)
	}
}
