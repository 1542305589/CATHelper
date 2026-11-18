package detector_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/Computing-Availability-Tools/CATHelper/feature/straggler/config"
	"github.com/Computing-Availability-Tools/CATHelper/feature/straggler/profiling/detector"
	"github.com/Computing-Availability-Tools/CATHelper/feature/straggler/report"
	"github.com/Computing-Availability-Tools/CATHelper/feature/straggler/utils"
)

// End-to-end level0 regression: one slow card, one slow comm group and one slow
// CPU node must flow through detection → node aggregation → Python-schema result
// file with the expected keys.
func TestLevel0ParityEndToEnd(t *testing.T) {
	dir := t.TempDir()
	metricDir := filepath.Join(dir, "op_metric")
	if err := os.MkdirAll(metricDir, 0o755); err != nil {
		t.Fatal(err)
	}
	nodes := []string{"nodeA", "nodeA", "nodeB", "nodeB"}
	for rank, node := range nodes {
		hi := `{"rank":"` + itoa(rank) + `","hostName":"` + node + `","hostUid":"` + node + `"}`
		if err := os.WriteFile(filepath.Join(metricDir, "host_info_"+itoa(rank)+".json"), []byte(hi), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(metricDir, "npu_info_"+itoa(rank)+".json"), []byte(`{"id":`+itoa(rank%2)+`}`), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	oldPath := config.FilePath
	config.FilePath = dir
	defer func() { config.FilePath = oldPath }()

	config.CalThreshold = 1.3
	config.CPUThreshold = 2.5
	config.CommThreshold = 1.3
	config.SlowCommFlat = false
	config.PPSessionNs = 0
	defer func() {
		config.CalThreshold, config.CPUThreshold, config.CommThreshold = 0, 0, 0
	}()

	step := map[string]map[int]float64{
		"KERNEL_AICORE":      {0: 10, 1: 10, 2: 10, 3: 40},
		"tp_allReduce_20480": {0: 10, 1: 10, 2: 5, 3: 5},
		"ZP_Host":            {0: 10, 1: 10, 2: 100, 3: 100},
	}
	parallels := map[string][][]int{"tp": {{0, 1}, {2, 3}}}
	validRanks := []int{0, 1, 2, 3}

	result := detector.DelimitDetection(step, parallels, validRanks)
	if _, ok := result["cal"]["3"]; !ok {
		t.Fatalf("cal = %v, want rank 3", result["cal"])
	}
	if _, ok := result["comm"]["2,3"]; !ok {
		t.Fatalf("comm = %v, want group 2,3", result["comm"])
	}
	if _, ok := result["cpu"]["nodeB"]; !ok {
		t.Fatalf("cpu = %v, want nodeB", result["cpu"])
	}

	nodeOut, err := utils.BuildNodeResult(result, parallels, nil)
	if err != nil {
		t.Fatalf("BuildNodeResult: %v", err)
	}
	if len(nodeOut.NodeResult) != 1 || nodeOut.NodeResult[0].Hostname != "nodeB" {
		t.Fatalf("node_result = %+v, want only nodeB (abnormal nodes only)", nodeOut.NodeResult)
	}

	outDir := filepath.Join(dir, "out")
	if err := report.WriteDetectionResult(outDir, result, parallels); err != nil {
		t.Fatalf("WriteDetectionResult: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(outDir, report.DetectionResultFile))
	if err != nil {
		t.Fatalf("read result: %v", err)
	}
	var doc map[string][]map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	wantKeys := map[string]string{
		"KERNEL_AICORE": "3",
		"comm":          "tp[2, 3]",
		"cpu":           "nodeB",
	}
	for cat, key := range wantKeys {
		items := doc[cat]
		if len(items) != 1 {
			t.Fatalf("%s = %v, want exactly one item", cat, items)
		}
		if items[0]["display_key"] != key {
			t.Fatalf("%s display_key = %v, want %q", cat, items[0]["display_key"], key)
		}
	}
}

// itoa is a tiny helper to avoid importing strconv in this test's fixtures.
func itoa(v int) string {
	if v == 0 {
		return "0"
	}
	var buf [12]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	return string(buf[i:])
}
