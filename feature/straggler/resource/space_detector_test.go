package resource

import (
	"testing"
)

// =============================================================================
// detectSpaceAnomalies — MethodDirect (aicore_freq) peer-min regression tests
// =============================================================================

// freqRows builds one detection row with the given per-card aicore_freq.
// Cards absent from the map are absent from the row entirely.
func freqRows(freqs map[int]float64) []CSVRow {
	row := CSVRow{Timestamp: 1_000_000, AICoreFreq: freqs}
	return []CSVRow{row}
}

func freqCardIDs(n int) []int {
	ids := make([]int, n)
	for i := range ids {
		ids[i] = i
	}
	return ids
}

// Single downclocked card must be flagged: 1000MHz vs peers at 1800MHz.
// Regression: the peer min used to include the card itself, so the lowest
// card (which IS the global min) could never satisfy "below min of others".
func TestSpaceFreqSingleDownclock(t *testing.T) {
	cfg := DefaultDetectionConfig()
	cardIDs := freqCardIDs(8)
	freqs := map[int]float64{0: 1800, 1: 1800, 2: 1800, 3: 1800, 4: 1800, 5: 1800, 6: 1800, 7: 1000}

	res := detectSpaceAnomalies(freqRows(freqs), cardIDs, cfg)
	z := res.Scores[7][MetricAICoreFreq][0]
	if z != 999 {
		t.Fatalf("downclocked card 7 → space z = %v, want 999", z)
	}
	for cid := 0; cid < 7; cid++ {
		if z := res.Scores[cid][MetricAICoreFreq][0]; z != 0 {
			t.Errorf("normal card %d → space z = %v, want 0", cid, z)
		}
	}

	details := aggregateSpaceScores(res, cardIDs, cfg)
	if d := details[7][MetricAICoreFreq]; !d.SpaceAbnormal {
		t.Errorf("card 7 spaceAbnormal = false, want true (score=%v)", d.SpaceScore)
	}
	if d := details[0][MetricAICoreFreq]; d.SpaceAbnormal {
		t.Errorf("card 0 spaceAbnormal = true, want false")
	}
}

// All cards on the same clock level → nobody flagged.
func TestSpaceFreqAllNormal(t *testing.T) {
	cfg := DefaultDetectionConfig()
	cardIDs := freqCardIDs(4)
	freqs := map[int]float64{0: 1800, 1: 1800, 2: 1800, 3: 1800}

	res := detectSpaceAnomalies(freqRows(freqs), cardIDs, cfg)
	for _, cid := range cardIDs {
		if z := res.Scores[cid][MetricAICoreFreq][0]; z != 0 {
			t.Errorf("card %d at common clock → z = %v, want 0", cid, z)
		}
	}
}

// Two cards downclocked to the SAME value are not uniquely slower than each
// other → neither is flagged in space. The time dimension catches this.
func TestSpaceFreqTiedDownclock(t *testing.T) {
	cfg := DefaultDetectionConfig()
	cardIDs := freqCardIDs(8)
	freqs := map[int]float64{0: 1800, 1: 1800, 2: 1800, 3: 1800, 4: 1800, 5: 1800, 6: 1000, 7: 1000}

	res := detectSpaceAnomalies(freqRows(freqs), cardIDs, cfg)
	for _, cid := range []int{6, 7} {
		if z := res.Scores[cid][MetricAICoreFreq][0]; z != 0 {
			t.Errorf("tied-downclocked card %d → z = %v, want 0", cid, z)
		}
	}
}

// A card absent from the row must not be flagged.
func TestSpaceFreqAbsentCardNotFlagged(t *testing.T) {
	cfg := DefaultDetectionConfig()
	cardIDs := freqCardIDs(4)
	freqs := map[int]float64{0: 1800, 1: 1800, 2: 1800} // card 3 absent

	res := detectSpaceAnomalies(freqRows(freqs), cardIDs, cfg)
	if z := res.Scores[3][MetricAICoreFreq][0]; z != 0 {
		t.Errorf("absent card 3 → z = %v, want 0", z)
	}
}

// Downclock within the FreqDownclockGap tolerance is natural spread, not anomaly.
func TestSpaceFreqWithinGapTolerance(t *testing.T) {
	cfg := DefaultDetectionConfig() // FreqDownclockGap = 200
	cardIDs := freqCardIDs(4)
	freqs := map[int]float64{0: 1800, 1: 1800, 2: 1800, 3: 1700}

	res := detectSpaceAnomalies(freqRows(freqs), cardIDs, cfg)
	if z := res.Scores[3][MetricAICoreFreq][0]; z != 0 {
		t.Errorf("card 3 within gap tolerance → z = %v, want 0", z)
	}
}

// =============================================================================
// detectSpaceAnomalies — MethodCluster (kmeans ratio) tests
// =============================================================================

// clusterTempRows builds detection rows from per-row temp patterns (one slice
// per row, aligned with cardIDs 0..n-1).
func clusterTempRows(patterns [][]float64) []CSVRow {
	var rows []CSVRow
	ts := int64(1_000_000)
	for _, pat := range patterns {
		m := make(map[int]float64, len(pat))
		for i, v := range pat {
			m[i] = v
		}
		rows = append(rows, CSVRow{Timestamp: ts, Temp: m})
		ts += 60
	}
	return rows
}

func clusterCardIDs(n int) []int {
	return freqCardIDs(n)
}

// Single anomalous card (100°C vs 30°C peers, ratio 3.33 > 2.0) must be flagged.
func TestSpaceClusterSingleAnomaly(t *testing.T) {
	cfg := DefaultDetectionConfig() // SpaceRatioThreshold = 2.0
	cardIDs := clusterCardIDs(8)

	rows := clusterTempRows([][]float64{{30, 30, 30, 30, 30, 30, 30, 100}})
	res := detectSpaceAnomalies(rows, cardIDs, cfg)
	details := aggregateSpaceScores(res, cardIDs, cfg)

	if !details[7][MetricTemp].SpaceAbnormal {
		t.Errorf("hot card 7 should be space-abnormal (score=%v)", details[7][MetricTemp].SpaceScore)
	}
	if got := details[7][MetricTemp].SpaceScore; got < 3.33 || got > 3.34 {
		t.Errorf("hot card 7 space_score = %v, want ≈3.33", got)
	}
	for cid := 0; cid < 7; cid++ {
		if details[cid][MetricTemp].SpaceAbnormal {
			t.Errorf("normal card %d should not be space-abnormal", cid)
		}
	}
}

// Two anomalous cards (both 100°C) must BOTH be flagged — the multi-card case
// that the old mean/std z-score diluted.
func TestSpaceClusterMultiAnomaly(t *testing.T) {
	cfg := DefaultDetectionConfig()
	cardIDs := clusterCardIDs(8)

	rows := clusterTempRows([][]float64{{30, 30, 30, 30, 30, 30, 100, 100}})
	res := detectSpaceAnomalies(rows, cardIDs, cfg)
	details := aggregateSpaceScores(res, cardIDs, cfg)

	for _, cid := range []int{6, 7} {
		if !details[cid][MetricTemp].SpaceAbnormal {
			t.Errorf("hot card %d should be space-abnormal (score=%v)", cid, details[cid][MetricTemp].SpaceScore)
		}
	}
	for cid := 0; cid < 6; cid++ {
		if details[cid][MetricTemp].SpaceAbnormal {
			t.Errorf("normal card %d should not be space-abnormal", cid)
		}
	}
}

// A spread fleet (54..60, max ratio 60/54 ≈ 1.11 < 2.0) has no cluster whose
// mean ratio exceeds the threshold → nobody flagged. Natural spread must not
// be treated as an anomaly.
func TestSpaceClusterMajorityNormalSpread(t *testing.T) {
	cfg := DefaultDetectionConfig()
	cardIDs := clusterCardIDs(8)

	rows := clusterTempRows([][]float64{{54, 55, 55, 56, 57, 58, 59, 60}})
	res := detectSpaceAnomalies(rows, cardIDs, cfg)
	details := aggregateSpaceScores(res, cardIDs, cfg)

	for _, cid := range cardIDs {
		if details[cid][MetricTemp].SpaceAbnormal {
			t.Errorf("card %d in a normal spread should not be space-abnormal (score=%v)",
				cid, details[cid][MetricTemp].SpaceScore)
		}
	}
}

// A mild fleet-wide shift (60 vs 55, ratio 1.09 < 2.0) stays silent even when
// the higher group is the majority — the ratio threshold, not majority
// membership, decides.
func TestSpaceClusterMajorityAnomaly(t *testing.T) {
	cfg := DefaultDetectionConfig()
	cardIDs := clusterCardIDs(8)

	rows := clusterTempRows([][]float64{{60, 60, 60, 60, 60, 55, 55, 55}})
	res := detectSpaceAnomalies(rows, cardIDs, cfg)
	details := aggregateSpaceScores(res, cardIDs, cfg)

	for _, cid := range cardIDs {
		if details[cid][MetricTemp].SpaceAbnormal {
			t.Errorf("card %d: mild fleet-wide shift must stay silent (score=%v)",
				cid, details[cid][MetricTemp].SpaceScore)
		}
	}
}

// A 4/4 tie picks the direction extreme (lower mean for DirHigh) as baseline,
// so the hot half is still flagged — no midpoint dilution.
func TestSpaceClusterTieBaseline(t *testing.T) {
	cfg := DefaultDetectionConfig()
	cardIDs := clusterCardIDs(8)

	rows := clusterTempRows([][]float64{{30, 30, 30, 30, 80, 80, 80, 80}})
	res := detectSpaceAnomalies(rows, cardIDs, cfg)
	details := aggregateSpaceScores(res, cardIDs, cfg)

	for _, cid := range []int{4, 5, 6, 7} {
		if !details[cid][MetricTemp].SpaceAbnormal {
			t.Errorf("hot card %d (tie baseline) should be space-abnormal", cid)
		}
	}
	for cid := 0; cid < 4; cid++ {
		if details[cid][MetricTemp].SpaceAbnormal {
			t.Errorf("cool card %d should not be space-abnormal", cid)
		}
	}
}

// DirLow metric (aicore_util): cards idle at 30% vs working 90% peers → the
// low cluster is flagged (one-sided, baseline = direction extreme = working).
func TestSpaceClusterDirLow(t *testing.T) {
	cfg := DefaultDetectionConfig()
	cardIDs := clusterCardIDs(8)

	// 6 cards working at 90%, 2 cards at 30% (util dropped, ratio 3.0 > 2.0).
	rows := []CSVRow{{
		Timestamp:  1_000_000,
		AICoreUtil: map[int]float64{0: 90, 1: 90, 2: 90, 3: 90, 4: 90, 5: 90, 6: 30, 7: 30},
	}}
	res := detectSpaceAnomalies(rows, cardIDs, cfg)
	details := aggregateSpaceScores(res, cardIDs, cfg)

	for _, cid := range []int{6, 7} {
		if !details[cid][MetricAICoreUtil].SpaceAbnormal {
			t.Errorf("low-util card %d should be space-abnormal (score=%v)",
				cid, details[cid][MetricAICoreUtil].SpaceScore)
		}
	}
	for cid := 0; cid < 6; cid++ {
		if details[cid][MetricAICoreUtil].SpaceAbnormal {
			t.Errorf("working card %d should not be space-abnormal", cid)
		}
	}
}

// Space detection judges ONLY the last aggregated minute point: an anomaly in
// an earlier row (but a clean last row) is not flagged, and a clean earlier
// row followed by an anomalous last row IS flagged.
func TestSpaceClusterLastPointOnly(t *testing.T) {
	cfg := DefaultDetectionConfig()
	cardIDs := clusterCardIDs(8)

	// Anomaly in the first row only → last row is clean → nothing flagged.
	rowsCleanLast := clusterTempRows([][]float64{
		{30, 30, 30, 30, 30, 30, 30, 100},
		{30, 30, 30, 30, 30, 30, 30, 30},
	})
	res := detectSpaceAnomalies(rowsCleanLast, cardIDs, cfg)
	details := aggregateSpaceScores(res, cardIDs, cfg)
	if details[7][MetricTemp].SpaceAbnormal {
		t.Errorf("card 7 flagged although the LAST point was normal")
	}

	// Clean first row, anomalous last row → flagged.
	rowsLast := clusterTempRows([][]float64{
		{30, 30, 30, 30, 30, 30, 30, 30},
		{30, 30, 30, 30, 30, 30, 30, 100},
	})
	resLast := detectSpaceAnomalies(rowsLast, cardIDs, cfg)
	detailsLast := aggregateSpaceScores(resLast, cardIDs, cfg)
	if !detailsLast[7][MetricTemp].SpaceAbnormal {
		t.Errorf("card 7 should be flagged on the last point (score=%v)",
			detailsLast[7][MetricTemp].SpaceScore)
	}
	if got := len(resLast.Scores[7][MetricTemp]); got != 1 {
		t.Errorf("score array has %d elements, want exactly 1 (last point only)", got)
	}
}
