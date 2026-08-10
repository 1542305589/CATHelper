package resource

import (
	"encoding/csv"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// =============================================================================
// cardIndexer
// =============================================================================

func TestCardIndexer(t *testing.T) {
	idx := newCardIndexer()

	// Flat input: global ID == raw card ID (backward compat).
	if g := idx.globalID(noneNode, 3); g != 3 {
		t.Fatalf("flat card 3 → global %d, want 3", g)
	}
	// Same (node, card) is stable across calls.
	if g := idx.globalID(noneNode, 3); g != 3 {
		t.Fatalf("flat card 3 re-fetch → global %d, want 3", g)
	}

	// Named nodes get independent sequential numbering (each node from 0).
	ga0 := idx.globalID("node-a", 0)
	gb0 := idx.globalID("node-b", 0)
	if ga0 == gb0 {
		t.Fatalf("node-a card 0 and node-b card 0 must not collide: %d", ga0)
	}
	if idx.localMap()[ga0] != 0 || idx.localMap()[gb0] != 0 {
		t.Fatalf("both nodes' card 0 should map to local 0")
	}
	if idx.nodeMap()[ga0] != "node-a" || idx.nodeMap()[gb0] != "node-b" {
		t.Fatalf("node map wrong: %v", idx.nodeMap())
	}

	// Same (node, card) stable for named nodes too.
	if g := idx.globalID("node-a", 0); g != ga0 {
		t.Fatalf("node-a card 0 re-fetch → global %d, want %d", g, ga0)
	}
}

// =============================================================================
// ParseCSV — nested node JSON
// =============================================================================

func TestParseCSVNodeNested(t *testing.T) {
	// node-a has cards 0,1; node-b has cards 0,1 (per-node numbering).
	csvPath := filepath.Join(t.TempDir(), "nested.csv")
	f, err := os.Create(csvPath)
	if err != nil {
		t.Fatal(err)
	}
	w := csv.NewWriter(f)
	w.Write([]string{"timestamp", "NPU_CARD_TEMP"})
	w.Write([]string{"1000", `{"node-a":{"0":55,"1":56},"node-b":{"0":60,"1":61}}`})
	w.Flush()
	f.Close()

	ts, err := ParseCSV(csvPath)
	if err != nil {
		t.Fatalf("ParseCSV: %v", err)
	}
	if len(ts.CardIDs) != 4 {
		t.Fatalf("expected 4 global cards, got %v", ts.CardIDs)
	}
	row := ts.Rows[0]
	// Global IDs: node-a cards 0,1 and node-b cards 0,1 must all be distinct.
	seen := map[int]bool{}
	for _, cid := range ts.CardIDs {
		if seen[cid] {
			t.Fatalf("duplicate global card ID %d", cid)
		}
		seen[cid] = true
	}
	// Local IDs are per-node from 0.
	for _, cid := range ts.CardIDs {
		if ts.LocalID[cid] != 0 && ts.LocalID[cid] != 1 {
			t.Fatalf("card %d local %d, want 0 or 1", cid, ts.LocalID[cid])
		}
	}
	// Node assignment: find the card whose local ID is 1 in node-a → 56.
	var found bool
	for _, cid := range ts.CardIDs {
		if ts.NodeOf[cid] == "node-a" && ts.LocalID[cid] == 1 {
			if row.Temp[cid] != 56 {
				t.Errorf("node-a card 1 temp = %v, want 56", row.Temp[cid])
			}
			found = true
		}
	}
	if !found {
		t.Errorf("did not find node-a card 1 in parsed rows")
	}
}

// =============================================================================
// Space detection — peer comparison within a node only
// =============================================================================

func TestSpaceClusterPerNode(t *testing.T) {
	cfg := DefaultDetectionConfig()
	cardIDs := []int{0, 1, 2, 3}
	baselines := clusterBaselines(cardIDs, MetricTemp, 1.0) // scale = 1.4826
	nodeOf := map[int]string{0: "node-a", 1: "node-a", 2: "node-b", 3: "node-b"}

	// node-a: both 55 (normal). node-b: card 2 at 55, card 3 at 80 (hot).
	rows := []CSVRow{{Timestamp: 1_000_000, Temp: map[int]float64{0: 55, 1: 55, 2: 55, 3: 80}}}
	res := detectSpaceAnomalies(rows, baselines, cardIDs, cfg, nodeOf)
	details := aggregateSpaceScores(res, cardIDs, cfg)

	if !details[3][MetricTemp].SpaceAbnormal {
		t.Errorf("node-b card 3 (hot) should be space-abnormal, score=%v",
			details[3][MetricTemp].SpaceScore)
	}
	for _, cid := range []int{0, 1, 2} {
		if details[cid][MetricTemp].SpaceAbnormal {
			t.Errorf("card %d should NOT be space-abnormal (node-b card 2 must not be polluted by card 3)", cid)
		}
	}
	// node-b card 2 (local 0) is the baseline in its node; card 0 in node-a is
	// normal in its own node. Both stay clean even though card 3 is hot.
	if details[2][MetricTemp].SpaceAbnormal {
		t.Errorf("node-b card 2 is the majority member → should be exempt")
	}
}

// =============================================================================
// MarshalJSON — method-aware field names
// =============================================================================

func TestMetricAnomalyDetailMarshalJSON(t *testing.T) {
	// MAD time + cluster space → current_median/baseline_median/baseline_mad/cluster_mean.
	mad := MetricAnomalyDetail{
		Metric:        MetricTemp,
		SpaceScore:    16.9,
		TimeScore:     3.37,
		FusionScore:   8.8,
		SpaceAbnormal: true,
		TimeAbnormal:  false,
		Quadrant:      QuadIndividualVariance,
		CurrentMean:   80,
		BaselineMean:  55,
		BaselineStd:   1.4826,
		SpaceMethod:   MethodCluster,
		TimeMethod:    MethodMAD,
		SpaceRef:      55,
	}
	b, err := json.Marshal(mad)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]interface{}
	json.Unmarshal(b, &m)
	for _, k := range []string{"current_median", "time_baseline_median", "time_baseline_mad", "space_baseline_mean", "space_scale", "space_method", "time_method"} {
		if _, ok := m[k]; !ok {
			t.Errorf("MAD/cluster detail missing key %q in %s", k, b)
		}
	}
	for _, k := range []string{"current_mean", "time_baseline_std", "peer_mean"} {
		if _, ok := m[k]; ok {
			t.Errorf("MAD/cluster detail should NOT have %q in %s", k, b)
		}
	}
	// Key order must be preserved (not alphabetized): metric first, then the
	// method-aware fields in declaration order.
	if got := string(b); len(got) < 1 || !strings.HasPrefix(got, `{"metric":`) {
		t.Errorf("detail JSON should start with {\"metric\":..., got %s", got)
	}
	if idxMetric := strings.Index(got, `"metric"`); idxMetric < 0 || strings.Index(got, `"space_score"`) < idxMetric {
		t.Errorf("metric must come before space_score in %s", got)
	}
	if idxSpace := strings.Index(got, `"space_baseline_mean"`); idxSpace < 0 {
		t.Errorf("space_baseline_mean missing in %s", got)
	}
	if idxBaseline := strings.Index(got, `"time_baseline_mad"`); idxBaseline < 0 || idxBaseline > idxSpace {
		t.Errorf("time_baseline_mad must come before space_baseline_mean in %s", got)
	}

	// Mean/std time + direct space → current_mean/baseline_mean/baseline_std/peer_mean.
	zs := MetricAnomalyDetail{
		Metric:       MetricTXBandwidth,
		CurrentMean:  100,
		BaselineMean: 110,
		BaselineStd:  5,
		PeerMean:     105,
		SpaceMethod:  MethodDirect,
		TimeMethod:   MethodZScore,
	}
	b2, err := json.Marshal(zs)
	if err != nil {
		t.Fatal(err)
	}
	var m2 map[string]interface{}
	json.Unmarshal(b2, &m2)
	for _, k := range []string{"current_mean", "time_baseline_mean", "time_baseline_std", "peer_mean"} {
		if _, ok := m2[k]; !ok {
			t.Errorf("zscore/direct detail missing key %q in %s", k, b2)
		}
	}
	for _, k := range []string{"current_median", "space_baseline_mean", "space_scale"} {
		if _, ok := m2[k]; ok {
			t.Errorf("zscore/direct detail should NOT have %q in %s", k, b2)
		}
	}
}

// =============================================================================
// applyNodeIdentity
// =============================================================================

func TestApplyNodeIdentity(t *testing.T) {
	nodeOf := map[int]string{5: "node-a", 6: "node-b"}
	localID := map[int]int{5: 1, 6: 0}

	summaries := []CardDetectionSummary{
		{CardID: 5, Quadrant: QuadConfirmedAnomaly},
		{CardID: 6, Quadrant: QuadNormal},
	}
	rootCauses := []RootCauseResult{{CardID: 5, Category: RcThermalThrottle}}

	s, r := applyNodeIdentity(summaries, rootCauses, nodeOf, localID)

	if s[0].Node != "node-a" || s[0].CardID != 1 {
		t.Errorf("summary[0] → node=%s card=%d, want node-a/1", s[0].Node, s[0].CardID)
	}
	if s[1].Node != "node-b" || s[1].CardID != 0 {
		t.Errorf("summary[1] → node=%s card=%d, want node-b/0", s[1].Node, s[1].CardID)
	}
	if s[0].Quadrant != QuadConfirmedAnomaly {
		t.Errorf("quadrant must be preserved through conversion, got %v", s[0].Quadrant)
	}
	if len(r) != 1 || r[0].Node != "node-a" || r[0].CardID != 1 {
		t.Errorf("root cause not converted: %+v", r)
	}
}

func TestApplyNodeIdentityMissingDefaultsToNone(t *testing.T) {
	s, _ := applyNodeIdentity(
		[]CardDetectionSummary{{CardID: 9}},
		[]RootCauseResult{},
		map[int]string{}, map[int]int{},
	)
	if s[0].Node != noneNode {
		t.Errorf("missing node map → node %q, want %q", s[0].Node, noneNode)
	}
}
