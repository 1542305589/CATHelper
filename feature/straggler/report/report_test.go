package report

import (
	"strings"
	"testing"
)

// Degraded (no named domain) runs still detect compute, aivec/memcpy and CPU;
// the report summary must not hide those categories, only the comm ones.
func TestDetectionSummaryCalOnlyKeepsComputeCategories(t *testing.T) {
	result := map[string]map[string]float64{
		"cal":          {"2": 4.0},
		"memcpy_async": {"3": 3.0},
		"cpu":          {"nodeB": 10.0},
	}
	out := detectionSummary(result, nil, []int{0, 1, 2, 3}, true)
	for _, want := range []string{"慢计算 (cal)", "搬运 (memcpy_async)", "慢CPU (cpu)"} {
		if !strings.Contains(out, want) {
			t.Fatalf("summary missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "慢通信 (comm)") {
		t.Fatalf("summary should omit comm in degraded mode:\n%s", out)
	}
	if !strings.Contains(out, "PP通信 未评估") {
		t.Fatalf("summary missing degraded note:\n%s", out)
	}
}
