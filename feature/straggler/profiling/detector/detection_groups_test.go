package detector

import (
	"testing"

	"github.com/Computing-Availability-Tools/CATHelper/feature/straggler/config"
)

// Without any named parallel domain, detection groups degrade to physical-node
// groups built from hostUid.
func TestHostUidFallbackGroups(t *testing.T) {
	hostUid := map[int]string{0: "A", 1: "A", 2: "B", 3: "B"}
	name, groups := GetCalDetectionGroup(nil, []int{0, 1, 2, 3}, hostUid)
	if name != "" {
		t.Fatalf("group name = %q, want empty (hostUid fallback)", name)
	}
	want := [][]int{{0, 1}, {2, 3}}
	if len(groups) != len(want) {
		t.Fatalf("groups = %v, want %v", groups, want)
	}
	for i := range want {
		if len(groups[i]) != len(want[i]) || groups[i][0] != want[i][0] || groups[i][1] != want[i][1] {
			t.Fatalf("groups = %v, want %v", groups, want)
		}
	}
}

// Comm detection runs only when named domains exist.
func TestCommGatedByHasNamedDomain(t *testing.T) {
	resetSlowCommConfig()
	config.CommThreshold = 1.3
	config.HasNamedDomain = false
	defer func() {
		config.CommThreshold = 0
		config.HasNamedDomain = false
	}()

	step := map[string]map[int]float64{
		"KERNEL_AICORE":      {0: 10, 1: 10, 2: 10, 3: 10},
		"tp_allReduce_20480": {0: 5, 1: 5, 2: 20, 3: 20},
	}
	res := DelimitDetection(step, nil, []int{0, 1, 2, 3})
	if len(res["comm"]) != 0 {
		t.Fatalf("comm = %v, want none without named domain", res["comm"])
	}

	config.HasNamedDomain = true
	res = DelimitDetection(step, map[string][][]int{"tp": {{0, 1}, {2, 3}}}, []int{0, 1, 2, 3})
	if len(res["comm"]) == 0 {
		t.Fatalf("comm = %v, want an anomaly with named domain", res["comm"])
	}
}
