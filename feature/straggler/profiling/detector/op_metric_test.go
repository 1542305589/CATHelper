package detector

import "testing"

func TestBuildDetectionInput(t *testing.T) {
	op := OpMetric{
		"0": {
			GroupInfo: map[string]any{
				"g1": map[string]any{"group_name": "tp", "global_ranks": []any{float64(0), float64(1)}},
			},
			HostInfo: map[string]any{"rank": "0", "hostUid": "node-1", "hostName": "master"},
			NpuInfo:  map[string]any{"rank": "0", "id": float64(0)},
			GlobalRank: map[string]any{
				"StepIndex":  float64(0),
				"ZP_Kernel":  float64(100),
				"tp_Duration": float64(50),
			},
		},
		"1": {
			GroupInfo: map[string]any{
				"g1": map[string]any{"group_name": "tp", "global_ranks": []any{float64(0), float64(1)}},
			},
			HostInfo: map[string]any{"rank": "1", "hostUid": "node-1", "hostName": "master"},
			NpuInfo:  map[string]any{"rank": "1", "id": float64(1)},
			GlobalRank: map[string]any{
				"StepIndex":  float64(0),
				"ZP_Kernel":  float64(200),
				"tp_Duration": float64(60),
			},
		},
	}

	parallels, validRanks, stepData, hostUid := BuildDetectionInput(op)

	if len(validRanks) != 2 || validRanks[0] != 0 || validRanks[1] != 1 {
		t.Fatalf("validRanks = %v, want [0 1]", validRanks)
	}
	if got := parallels["tp"]; len(got) != 1 || len(got[0]) != 2 {
		t.Fatalf("parallels[tp] = %v, want [[0 1]]", got)
	}
	if got := stepData["ZP_Kernel"]; got[0] != 100 || got[1] != 200 {
		t.Fatalf("stepData[ZP_Kernel] = %v, want {0:100 1:200}", got)
	}
	if got := stepData["tp_Duration"]; got[0] != 50 || got[1] != 60 {
		t.Fatalf("stepData[tp_Duration] = %v, want {0:50 1:60}", got)
	}
	if _, ok := stepData["StepIndex"]; ok {
		t.Fatalf("StepIndex should be skipped, got %v", stepData["StepIndex"])
	}
	if hostUid[0] != "node-1" || hostUid[1] != "node-1" {
		t.Fatalf("hostUid = %v, want both node-1", hostUid)
	}
}
