package detector

import (
	"testing"

	"github.com/Computing-Availability-Tools/CATHelper/feature/straggler/config"
)

func resetSlowCommConfig() {
	config.SlowCommRatio = 1.3
	config.SlowCommMinCount = 1000
}

// TestParseBandwidthCol verifies dynamic column-name parsing: it must accept
// "<opType>_<count>" bandwidth columns and reject the diagnostic Duration/Count
// columns and any non-numeric tail.
func TestParseBandwidthCol(t *testing.T) {
	prefix := "tp_"
	cases := []struct {
		col       string
		wantOp    string
		wantCount int
		wantOK    bool
	}{
		{"tp_ReduceScatter_8192", "ReduceScatter", 8192, true},
		{"tp_allReduce_2048", "allReduce", 2048, true},
		{"tp_Duration", "", 0, false},
		{"tp_Count", "", 0, false},
		{"tp_foo", "", 0, false},
		{"dp_allGather_4096", "", 0, false}, // wrong domain prefix
	}
	for _, c := range cases {
		op, cnt, ok := parseBandwidthCol(prefix, c.col)
		if ok != c.wantOK || op != c.wantOp || cnt != c.wantCount {
			t.Errorf("parseBandwidthCol(%q) = (%q,%d,%v), want (%q,%d,%v)",
				c.col, op, cnt, ok, c.wantOp, c.wantCount, c.wantOK)
		}
	}
}

func TestMagnitudeOf(t *testing.T) {
	cases := map[int]int{5: 1, 50: 10, 500: 100, 1000: 1000, 5000: 1000, 100000: 100000, 250000: 100000}
	for v, want := range cases {
		if got := magnitudeOf(v); got != want {
			t.Errorf("magnitudeOf(%d) = %d, want %d", v, got, want)
		}
	}
}

// TestDetectSlowDomainByBandwidth flags the low-bandwidth group via kmeans.
func TestDetectSlowDomainByBandwidth(t *testing.T) {
	resetSlowCommConfig()
	parallels := map[string][][]int{
		"tp": {{0, 1}, {2, 3}},
	}
	stepData := map[string]map[int]float64{
		"tp_allReduce_1000": {0: 5.0, 1: 5.0, 2: 20.0, 3: 20.0},
	}
	res := config.NewDegradationData()
	DetectSlowDomainByBandwidth(parallels, stepData, res)

	comm := res["comm"]
	if len(comm) != 1 {
		t.Fatalf("comm result = %v, want 1 entry", comm)
	}
	if v, ok := comm["0,1"]; !ok {
		t.Errorf("expected slow group 0,1, got %v", comm)
	} else if v < 1.0 {
		t.Errorf("degradation should be >1 (baseline/value), got %v", v)
	}
}

// TestDetectSlowDomainMaxCountRepresentative picks each group's largest-count
// entry as its bandwidth representative (ignoring a small-count anomaly).
func TestDetectSlowDomainMaxCountRepresentative(t *testing.T) {
	resetSlowCommConfig()
	parallels := map[string][][]int{
		"tp": {{0, 1}, {2, 3}},
	}
	stepData := map[string]map[int]float64{
		// group 0 slow only on the SMALL count; big count is normal.
		"tp_allReduce_1000": {0: 5.0, 1: 5.0, 2: 100.0, 3: 100.0},
		"tp_allReduce_2000": {0: 100.0, 1: 100.0, 2: 100.0, 3: 100.0},
	}
	res := config.NewDegradationData()
	DetectSlowDomainByBandwidth(parallels, stepData, res)
	if got := res["comm"]; len(got) != 0 {
		t.Errorf("expected no slow comm (representative = max count 2000), got %v", got)
	}
}

// TestDetectSlowDomainMagnitudeFilter drops groups whose representative count is
// below the max count's magnitude, so the tiny-count group never participates.
func TestDetectSlowDomainMagnitudeFilter(t *testing.T) {
	resetSlowCommConfig()
	parallels := map[string][][]int{
		"tp": {{0, 1}, {2, 3}, {4, 5}},
	}
	stepData := map[string]map[int]float64{
		// group 0 slow on count 5000; group 2 super-slow but count 500 (<1000).
		"tp_allReduce_5000": {0: 20, 1: 20, 2: 100, 3: 100},
		"tp_allReduce_500":  {4: 1, 5: 1},
	}
	res := config.NewDegradationData()
	DetectSlowDomainByBandwidth(parallels, stepData, res)

	comm := res["comm"]
	if len(comm) != 1 {
		t.Fatalf("comm result = %v, want 1 entry", comm)
	}
	if _, ok := comm["0,1"]; !ok {
		t.Errorf("expected slow group 0,1, got %v", comm)
	}
	if _, ok := comm["4,5"]; ok {
		t.Errorf("group 4,5 (count 500) should be filtered out, got %v", comm)
	}
}

// TestDetectSlowDomainReportsSingleGroup reports only ONE group per domain —
// the largest-degradation one — even when different opTypes flag different
// groups.
func TestDetectSlowDomainReportsSingleGroup(t *testing.T) {
	resetSlowCommConfig()
	parallels := map[string][][]int{
		"tp": {{0, 1}, {2, 3}, {4, 5}, {6, 7}},
	}
	stepData := map[string]map[int]float64{
		"tp_allReduce_5000": {0: 20, 1: 20, 2: 100, 3: 100, 4: 100, 5: 100, 6: 100, 7: 100},
		"tp_allGather_5000": {0: 100, 1: 100, 2: 50, 3: 50, 4: 100, 5: 100, 6: 100, 7: 100},
	}
	res := config.NewDegradationData()
	DetectSlowDomainByBandwidth(parallels, stepData, res)

	comm := res["comm"]
	if len(comm) != 1 {
		t.Fatalf("comm result = %v, want exactly 1 entry", comm)
	}
	if _, ok := comm["0,1"]; !ok {
		t.Fatalf("expected group 0,1 (allReduce 5x) to win over group 2,3 (allGather 2x), got %v", comm)
	}
}

// TestDetectSlowDomainByBandwidthNoSlow leaves near-equal bandwidth un-flagged.
func TestDetectSlowDomainByBandwidthNoSlow(t *testing.T) {
	resetSlowCommConfig()
	parallels := map[string][][]int{
		"tp": {{0, 1}, {2, 3}},
	}
	stepData := map[string]map[int]float64{
		"tp_allReduce_1000": {0: 100, 1: 100, 2: 101, 3: 101},
	}
	res := config.NewDegradationData()
	DetectSlowDomainByBandwidth(parallels, stepData, res)
	if got := res["comm"]; len(got) != 0 {
		t.Errorf("expected no slow comm, got %v", got)
	}
}

// TestDetectSlowDomainByBandwidthSkipsPP skips the point-to-point pp domain.
func TestDetectSlowDomainByBandwidthSkipsPP(t *testing.T) {
	resetSlowCommConfig()
	parallels := map[string][][]int{
		"pp": {{0, 1}, {2, 3}},
	}
	stepData := map[string]map[int]float64{
		"pp_allReduce_1000": {0: 5, 1: 5, 2: 20, 3: 20},
	}
	res := config.NewDegradationData()
	DetectSlowDomainByBandwidth(parallels, stepData, res)
	if got := res["comm"]; len(got) != 0 {
		t.Errorf("pp should be skipped, got %v", got)
	}
}