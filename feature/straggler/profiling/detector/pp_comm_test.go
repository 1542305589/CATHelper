package detector

import (
	"testing"

	"github.com/Computing-Availability-Tools/CATHelper/feature/straggler/config"
)

// Compare domains use TP first, then CP, then DP.
func TestPPCompareGroupsPriority(t *testing.T) {
	p := map[string][][]int{"tp": {{0, 1}}, "cp": {{2, 3}}}
	got := ppCompareGroups(p)
	if len(got) != 1 || len(got[0]) != 2 || got[0][0] != 0 || got[0][1] != 1 {
		t.Fatalf("compare groups = %v, want [[0 1]]", got)
	}
}

// Without TP/CP/DP, PP stage groups are transposed into per-stage groups.
func TestPPCompareGroupsStageTranspose(t *testing.T) {
	p := map[string][][]int{"pp": {{0, 1, 2}, {3, 4, 5}}}
	got := ppCompareGroups(p)
	want := [][]int{{0, 3}, {1, 4}, {2, 5}}
	if len(got) != len(want) {
		t.Fatalf("compare groups = %v, want %v", got, want)
	}
	for i := range want {
		if got[i][0] != want[i][0] || got[i][1] != want[i][1] {
			t.Fatalf("compare groups = %v, want %v", got, want)
		}
	}
}

// A rank 8% below the group maximum is reported at its PP group.
func TestDetectPPSlowDomain(t *testing.T) {
	p := map[string][][]int{"tp": {{0, 1}}, "pp": {{0, 1}}}
	step := map[string]map[int]float64{"PP_WaitGap": {0: 100, 1: 900}}
	res := config.NewDegradationData()
	DetectPPSlowDomain(p, step, res, 10000)
	v, ok := res["pp_comm"]["0,1"]
	if !ok {
		t.Fatalf("pp_comm = %v, want group 0,1", res["pp_comm"])
	}
	if v < 0.079 || v > 0.081 {
		t.Fatalf("pp_comm degradation = %v, want ~0.08", v)
	}
}

// PP detection is skipped when a compute anomaly was already found.
func TestPPSkippedOnComputeAnomaly(t *testing.T) {
	config.CalThreshold = 1.3
	config.CPUThreshold = 2.5
	config.PPSessionNs = 10000
	defer func() {
		config.CalThreshold, config.CPUThreshold, config.PPSessionNs = 0, 0, 0
	}()

	step := map[string]map[int]float64{
		"KERNEL_AICORE": {0: 10, 1: 10, 2: 40},
		"PP_WaitGap":    {0: 100, 1: 900},
	}
	p := map[string][][]int{"tp": {{0, 1, 2}}, "pp": {{0, 1}}}
	res := DelimitDetection(step, p, []int{0, 1, 2})
	if len(res["pp_comm"]) != 0 {
		t.Fatalf("pp_comm = %v, want none (compute anomaly present)", res["pp_comm"])
	}
}
