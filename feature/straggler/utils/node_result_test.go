package utils

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/Computing-Availability-Tools/CATHelper/feature/straggler/config"
)

// Level0: per-NPU aivec/memcpy anomalies, node-level CPU, comm domains and the
// PP comm result all land in the Go NodeOutput (no bubble field).
func TestBuildNodeResultLevel0(t *testing.T) {
	dir := t.TempDir()
	metricDir := filepath.Join(dir, "op_metric")
	if err := os.MkdirAll(metricDir, 0o755); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		"host_info_0.json": `{"rank":"0","hostName":"nodeA","hostUid":"A"}`,
		"host_info_1.json": `{"rank":"1","hostName":"nodeA","hostUid":"A"}`,
		"npu_info_0.json":  `{"id":0}`,
		"npu_info_1.json":  `{"id":1}`,
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(metricDir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	old := config.FilePath
	config.FilePath = dir
	defer func() { config.FilePath = old }()

	result := config.DegradationData{
		"cal":          {"0": 2.0},
		"kernel_aivec": {"1": 3.0},
		"memcpy_async": {"0": 2.5},
		"cpu":          {"nodeA": 4.0},
		"comm":         {"0,1": 1.5},
		"pp_comm":      {"0,1": 0.08},
	}
	out, err := BuildNodeResult(result, map[string][][]int{"tp": {{0, 1}}}, nil)
	if err != nil {
		t.Fatalf("BuildNodeResult: %v", err)
	}
	if len(out.NodeResult) != 1 || out.NodeResult[0].Hostname != "nodeA" {
		t.Fatalf("node_result = %+v, want one nodeA", out.NodeResult)
	}
	node := out.NodeResult[0]
	if node.CPU == nil || node.CPU.Score != 4.0 {
		t.Fatalf("cpu = %+v, want 4.0", node.CPU)
	}
	np := map[int]NpuResult{}
	for _, n := range node.Npu {
		np[n.ID] = n
	}
	if np[0].Cal == nil || np[0].MemcpyAsync == nil {
		t.Fatalf("npu0 = %+v, want cal + memcpy_async", np[0])
	}
	if np[1].KernelAivec == nil {
		t.Fatalf("npu1 = %+v, want kernel_aivec", np[1])
	}
	if out.CommDomainResult["tp"]["0,1"] != 1.5 {
		t.Fatalf("comm = %+v", out.CommDomainResult)
	}
	if out.PPCommResult["0,1"] != 0.08 {
		t.Fatalf("pp_comm = %+v", out.PPCommResult)
	}
}
