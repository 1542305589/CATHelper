package detector

import (
	"testing"

	"github.com/Computing-Availability-Tools/CATHelper/feature/straggler/config"
)

// Invalid ranks (0 / -99999) are dropped individually; the rest of the group is
// still compared (Python behavior), rather than skipping the whole group.
func TestDetMetricPerRankFilter(t *testing.T) {
	data := map[string]map[int]float64{"KERNEL_AICORE": {0: 10, 1: 0, 2: 40}}
	ranks, degs := detMetricForOneGroup(data, []int{0, 1, 2}, "KERNEL_AICORE", 1.3)
	if len(ranks) != 1 || ranks[0] != 2 {
		t.Fatalf("flagged ranks = %v, want [2]", ranks)
	}
	if len(degs) != 1 || degs[0] <= 1 {
		t.Fatalf("degradations = %v, want one > 1", degs)
	}
}

// kernel_aivec uses the compute threshold; memcpy_async uses the IO threshold.
func TestDetectAivecAndMemcpy(t *testing.T) {
	config.CalThreshold = 1.3
	config.CPUThreshold = 2.5
	defer func() { config.CalThreshold, config.CPUThreshold = 0, 0 }()

	aivec := map[string]map[int]float64{"KERNEL_AIVEC": {0: 10, 1: 10, 2: 40}}
	res := config.NewDegradationData()
	getSlowMetricRanks([][]int{{0, 1, 2}}, aivec, "KERNEL_AIVEC", "kernel_aivec", config.CalThreshold, res)
	if _, ok := res["kernel_aivec"]["2"]; !ok {
		t.Fatalf("kernel_aivec = %v, want rank 2", res["kernel_aivec"])
	}

	memcpy := map[string]map[int]float64{"MEMCPY_ASYNC": {0: 10, 1: 10, 2: 40}}
	res2 := config.NewDegradationData()
	getSlowMetricRanks([][]int{{0, 1, 2}}, memcpy, "MEMCPY_ASYNC", "memcpy_async", config.CPUThreshold, res2)
	if _, ok := res2["memcpy_async"]["2"]; !ok {
		t.Fatalf("memcpy_async = %v, want rank 2", res2["memcpy_async"])
	}
}

// CPU anomalies are reported once per physical node, keyed by node name.
func TestCpuNodeLevel(t *testing.T) {
	config.CPUThreshold = 2.5
	defer func() { config.CPUThreshold = 0 }()

	data := map[int]float64{0: 10, 1: 10, 2: 100, 3: 100}
	hostUid := map[int]string{0: "A", 1: "A", 2: "B", 3: "B"}
	nodeName := map[int]string{0: "nodeA", 1: "nodeA", 2: "nodeB", 3: "nodeB"}

	res := config.NewDegradationData()
	getSlowHostRanksByHomogenize([]int{0, 1, 2, 3}, data, res, hostUid, nodeName)

	cpu := res["cpu"]
	if len(cpu) != 1 {
		t.Fatalf("cpu = %v, want exactly one node", cpu)
	}
	if v, ok := cpu["nodeB"]; !ok || v <= 1 {
		t.Fatalf("cpu = %v, want nodeB > 1", cpu)
	}
}
