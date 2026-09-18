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

// TestDetectSlowDomainByBandwidth flags the low-bandwidth group via kmeans.
func TestDetectSlowDomainByBandwidth(t *testing.T) {
	resetSlowCommConfig()
	parallels := map[string][][]int{
		"tp": {{0, 1}, {2, 3}},
	}
	stepData := map[string]map[int]float64{
		"tp_allReduce_1000": {
			0: 5.0, 1: 5.0, // group [0,1] slow (5 vs 20)
			2: 20.0, 3: 20.0,
		},
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

// TestDetectSlowDomainByBandwidthNoSlow leaves near-equal bandwidth un-flagged.
func TestDetectSlowDomainByBandwidthNoSlow(t *testing.T) {
	resetSlowCommConfig()
	parallels := map[string][][]int{
		"tp": {{0, 1}, {2, 3}},
	}
	stepData := map[string]map[int]float64{
		"tp_allReduce_1000": {
			0: 100, 1: 100,
			2: 101, 3: 101, // ratio ~1.01 < 1.3
		},
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

// TestDetectSlowDomainMissingComboStillClusters verifies a combo clusters as
// long as ≥2 groups have it, even if other groups are missing that combo.
func TestDetectSlowDomainMissingComboStillClusters(t *testing.T) {
	resetSlowCommConfig()
	parallels := map[string][][]int{
		"tp": {{0, 1}, {2, 3}, {4, 5}},
	}
	stepData := map[string]map[int]float64{
		// only groups [0,1] and [2,3] have allReduce_1000; [4,5] is absent.
		"tp_allReduce_1000": {0: 5, 1: 5, 2: 20, 3: 20},
		// all three groups have allGather_2000, all equal → no anomaly.
		"tp_allGather_2000": {0: 10, 1: 10, 2: 10, 3: 10, 4: 10, 5: 10},
	}
	res := config.NewDegradationData()
	DetectSlowDomainByBandwidth(parallels, stepData, res)

	comm := res["comm"]
	if len(comm) != 1 {
		t.Fatalf("comm result = %v, want 1 entry (group 0,1)", comm)
	}
	if _, ok := comm["0,1"]; !ok {
		t.Errorf("expected slow group 0,1, got %v", comm)
	}
}

// TestDetectSlowDomainPicksHighestDegradation reports a group's largest ratio
// when it is slow on multiple combos.
func TestDetectSlowDomainPicksHighestDegradation(t *testing.T) {
	resetSlowCommConfig()
	parallels := map[string][][]int{
		"tp": {{0, 1}, {2, 3}},
	}
	stepData := map[string]map[int]float64{
		"tp_allReduce_1000": {0: 5, 1: 5, 2: 20, 3: 20},  // deg 4x
		"tp_allReduce_2000": {0: 10, 1: 10, 2: 20, 3: 20}, // deg 2x
	}
	res := config.NewDegradationData()
	DetectSlowDomainByBandwidth(parallels, stepData, res)

	comm := res["comm"]
	if len(comm) != 1 {
		t.Fatalf("comm result = %v, want 1 entry", comm)
	}
	v, ok := comm["0,1"]
	if !ok {
		t.Fatalf("expected slow group 0,1, got %v", comm)
	}
	if v < 3.9 || v > 4.1 {
		t.Errorf("degradation = %v, want ~4.0 (the highest of 4x/2x)", v)
	}
}

// TestDetectSlowDomainReportsSingleGroup reports only ONE group per domain —
// the one with the largest degradation — even when different combos flag
// different groups.
func TestDetectSlowDomainReportsSingleGroup(t *testing.T) {
	resetSlowCommConfig()
	parallels := map[string][][]int{
		"tp": {{0, 1}, {2, 3}, {4, 5}, {6, 7}},
	}
	stepData := map[string]map[int]float64{
		// group [0,1] slow on allReduce_1000 (deg ~4x).
		"tp_allReduce_1000": {0: 5, 1: 5, 2: 20, 3: 20, 4: 20, 5: 20, 6: 20, 7: 20},
		// group [2,3] slow on allGather_2000 (deg ~2x).
		"tp_allGather_2000": {0: 20, 1: 20, 2: 10, 3: 10, 4: 20, 5: 20, 6: 20, 7: 20},
	}
	res := config.NewDegradationData()
	DetectSlowDomainByBandwidth(parallels, stepData, res)

	comm := res["comm"]
	if len(comm) != 1 {
		t.Fatalf("comm result = %v, want exactly 1 entry (largest degradation)", comm)
	}
	v, ok := comm["0,1"]
	if !ok {
		t.Fatalf("expected group 0,1 (deg 4x) to win over group 2,3 (deg 2x), got %v", comm)
	}
	if v < 3.9 || v > 4.1 {
		t.Errorf("degradation = %v, want ~4.0", v)
	}
}
