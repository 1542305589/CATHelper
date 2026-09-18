package detector

import (
	"math"
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

// TestCompareBWGroupsDegradedA checks that the side with the lower bandwidth is
// counted as degraded on the matched combos and that no degradation is reported
// when the ratio is below the threshold.
func TestCompareBWGroupsDegradedA(t *testing.T) {
	resetSlowCommConfig()
	a := []bwEntry{
		{opType: "allReduce", count: 1000, bw: 5.0}, // degraded vs b
	}
	b := []bwEntry{
		{opType: "allReduce", count: 1000, bw: 20.0},
	}
	da, db, ra, rb := compareBWGroups(a, b, config.SlowCommRatio)
	if da != 1 || db != 0 {
		t.Errorf("degradedA=%d degradedB=%d, want 1/0", da, db)
	}
	if math.Abs(ra-4.0) > 1e-9 {
		t.Errorf("ratioA = %v, want 4.0", ra)
	}
	if rb != 1.0 {
		t.Errorf("ratioB = %v, want 1.0", rb)
	}
}

func TestCompareBWGroupsNoDegradation(t *testing.T) {
	resetSlowCommConfig()
	a := []bwEntry{{opType: "allReduce", count: 1000, bw: 20.0}}
	b := []bwEntry{{opType: "allReduce", count: 1000, bw: 22.0}} // ratio 1.1 < 1.3
	da, db, _, _ := compareBWGroups(a, b, config.SlowCommRatio)
	if da != 0 || db != 0 {
		t.Errorf("degradedA=%d degradedB=%d, want 0/0 (below threshold)", da, db)
	}
}

// TestCompareBWGroupsCountTolerance verifies that combos whose counts differ by
// more than 1.3x are not compared, and that only same-opType combos match.
func TestCompareBWGroupsCountTolerance(t *testing.T) {
	resetSlowCommConfig()
	// Good match within tolerance on same opType.
	a := []bwEntry{{opType: "allReduce", count: 1000, bw: 5.0}}
	b := []bwEntry{{opType: "allReduce", count: 1200, bw: 20.0}} // ratio 1.2 <= 1.3
	da, db, _, _ := compareBWGroups(a, b, config.SlowCommRatio)
	if da != 1 || db != 0 {
		t.Errorf("within-tolerance counts: degradedA=%d degradedB=%d, want 1/0", da, db)
	}

	// Count ratio > 1.3 -> not comparable.
	a2 := []bwEntry{{opType: "allReduce", count: 1000, bw: 5.0}}
	b2 := []bwEntry{{opType: "allReduce", count: 4000, bw: 20.0}} // ratio 4.0
	da2, db2, _, _ := compareBWGroups(a2, b2, config.SlowCommRatio)
	if da2 != 0 || db2 != 0 {
		t.Errorf("out-of-tolerance counts: degradedA=%d degradedB=%d, want 0/0", da2, db2)
	}

	// Different opType -> no match.
	a3 := []bwEntry{{opType: "allReduce", count: 1000, bw: 5.0}}
	b3 := []bwEntry{{opType: "allGather", count: 1000, bw: 20.0}}
	da3, db3, _, _ := compareBWGroups(a3, b3, config.SlowCommRatio)
	if da3 != 0 || db3 != 0 {
		t.Errorf("different opType: degradedA=%d degradedB=%d, want 0/0", da3, db3)
	}
}

func TestDetectSlowDomainByBandwidth(t *testing.T) {
	resetSlowCommConfig()
	parallels := map[string][][]int{
		"tp": {{0, 1}, {2, 3}},
	}
	stepData := map[string]map[int]float64{
		"tp_allReduce_1000": {
			0: 5.0, 1: 5.0, // group [0,1] slow
			2: 20.0, 3: 20.0,
		},
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
}

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
