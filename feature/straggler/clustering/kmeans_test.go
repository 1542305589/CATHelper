package clustering

import (
	"math"
	"testing"
)

// byIndex maps Results into a map[originalIndex]Ratio for lookup.
func byIndex(results []Result) map[int]float64 {
	m := make(map[int]float64, len(results))
	for _, r := range results {
		m[r.Index] = r.Ratio
	}
	return m
}

// assertFlagged checks that exactly the given original indices are flagged,
// each with a ratio within 1e-6 of the expected value.
func assertFlagged(t *testing.T, results []Result, expected map[int]float64) {
	t.Helper()
	got := byIndex(results)
	if len(got) != len(expected) {
		t.Fatalf("flagged %v, want %v", got, expected)
	}
	for idx, wantRatio := range expected {
		ratio, ok := got[idx]
		if !ok {
			t.Errorf("index %d not flagged, want ratio %.3f", idx, wantRatio)
			continue
		}
		if math.Abs(ratio-wantRatio) > 1e-6 {
			t.Errorf("index %d ratio = %.4f, want %.4f", idx, ratio, wantRatio)
		}
	}
}

// Single anomalous point (40 vs a 10-core majority) must be flagged at ~4x.
func TestDetectSingleAnomaly(t *testing.T) {
	vals := []float64{10, 10, 10, 10, 10, 10, 10, 40}
	res := Detect(vals, 2.0, true)
	assertFlagged(t, res, map[int]float64{7: 4.0})
}

// Multiple anomalous points (two 40s) must BOTH be flagged — the case that
// old mean/std scoring diluted.
func TestDetectMultiAnomaly(t *testing.T) {
	vals := []float64{10, 10, 10, 10, 10, 10, 40, 40}
	res := Detect(vals, 2.0, true)
	assertFlagged(t, res, map[int]float64{6: 4.0, 7: 4.0})
}

// A normally spread fleet (max internal ratio 60/54 ≈ 1.11 < 2.0) must flag
// nobody — the ratio threshold stops natural spread from being an anomaly even
// though kmeans always splits into ≥ 2 clusters.
func TestDetectAllNormalNoSplit(t *testing.T) {
	vals := []float64{54, 55, 55, 56, 57, 58, 59, 60}
	res := Detect(vals, 2.0, true)
	if len(res) != 0 {
		t.Fatalf("normal spread flagged %v, want none", res)
	}
}

// Bimodal fleet: a 4/4 tie picks the direction extreme (min mean) as baseline,
// so the high half is flagged — no midpoint dilution.
func TestDetectBimodal(t *testing.T) {
	vals := []float64{10, 10, 10, 10, 20, 20, 20, 20}
	res := Detect(vals, 1.5, true)
	assertFlagged(t, res, map[int]float64{4: 2.0, 5: 2.0, 6: 2.0, 7: 2.0})
}

// min direction: baseline = max-mean cluster; the low cluster is flagged with
// baseline/cluster ratio.
func TestDetectMinDirection(t *testing.T) {
	vals := []float64{40, 40, 40, 40, 40, 40, 10, 10}
	res := Detect(vals, 2.0, false)
	assertFlagged(t, res, map[int]float64{6: 4.0, 7: 4.0})
}

// Recursive replacement: the top-level anomaly cluster {40,40,60} recurses and
// isolates the 60 (ratio 1.5 > 1.3) as the deepest anomaly, replacing the
// parent — the 40s are dropped. With a threshold ≥ 1.5 the deeper split is
// silent (60/40 = 1.5 is not strictly greater), so the parent survives
// instead.
func TestDetectRecursiveReplacement(t *testing.T) {
	vals := []float64{10, 10, 10, 10, 40, 40, 60}
	res := Detect(vals, 1.3, true)
	assertFlagged(t, res, map[int]float64{6: 1.5})

	// Deeper silence keeps the parent: all three high cards flagged at the
	// parent-cluster ratio 46.7/10.
	resParent := Detect(vals, 1.5, true)
	assertFlagged(t, resParent, map[int]float64{4: 14.0 / 3, 5: 14.0 / 3, 6: 14.0 / 3})
}

// Recursive silence keeps the parent: an anomaly cluster with no internal
// structure reports all its members at the parent ratio.
func TestDetectRecursiveKeepsParent(t *testing.T) {
	vals := []float64{10, 10, 10, 10, 20, 20, 20}
	res := Detect(vals, 1.5, true)
	assertFlagged(t, res, map[int]float64{4: 2.0, 5: 2.0, 6: 2.0})
}

// Zero / negative / NaN values are ignored; < 2 positive values → no anomaly.
func TestDetectFiltersNonPositive(t *testing.T) {
	if res := Detect([]float64{0, 0, 0}, 2.0, true); len(res) != 0 {
		t.Fatalf("all-zero input flagged %v, want none", res)
	}
	if res := Detect([]float64{0, 10}, 2.0, true); len(res) != 0 {
		t.Fatalf("single positive input flagged %v, want none", res)
	}
	vals := []float64{0, 10, 0, 10, 10, 10, 10, 40, -5}
	res := Detect(vals, 2.0, true)
	assertFlagged(t, res, map[int]float64{7: 4.0})
}

// Empty / short inputs never panic and return nil.
func TestDetectEdgeInputs(t *testing.T) {
	if res := Detect(nil, 2.0, true); res != nil {
		t.Fatalf("nil input → %v, want nil", res)
	}
	if res := Detect([]float64{}, 2.0, true); res != nil {
		t.Fatalf("empty input → %v, want nil", res)
	}
	if res := Detect([]float64{5}, 2.0, true); res != nil {
		t.Fatalf("single value → %v, want nil", res)
	}
}
