package resource

import (
	"math"
	"testing"
)

// =============================================================================
// Robust statistics helpers
// =============================================================================

func TestMedian(t *testing.T) {
	cases := []struct {
		in   []float64
		want float64
	}{
		{[]float64{3, 1, 2}, 2},                        // odd n → exact middle
		{[]float64{4, 1, 3, 2}, 2.5},                   // even n → average of middle two
		{[]float64{80, 80, 80, 50, 80, 80}, 80},        // minority outlier does not move median
		{nil, 0},
	}
	for _, c := range cases {
		if got := Median(c.in); got != c.want {
			t.Errorf("Median(%v) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestMad(t *testing.T) {
	// Majority normal (80) with a minority fault (50): MAD reflects the normal
	// spread, not the outliers. Here deviations are {0×90, 30×10} → median 0.
	vals := make([]float64, 0, 100)
	for i := 0; i < 90; i++ {
		vals = append(vals, 80)
	}
	for i := 0; i < 10; i++ {
		vals = append(vals, 50)
	}
	if got := Mad(vals); got != 0 {
		t.Errorf("Mad(polluted constant) = %v, want 0", got)
	}

	// Small genuine spread around 80: MAD = 1.
	if got := Mad([]float64{78, 79, 80, 81, 82}); got != 1 {
		t.Errorf("Mad(spread) = %v, want 1", got)
	}
}

// =============================================================================
// BuildBaselines pollution robustness
// =============================================================================

// buildTempBaselineRows returns 100 baseline rows for card 0's temp metric:
// 90 normal minutes around 80°C + 10 polluted minutes (historical fault, 50°C).
func buildTempBaselineRows() []CSVRow {
	var rows []CSVRow
	ts := int64(1_000_000)
	for i := 0; i < 30; i++ {
		rows = append(rows, CSVRow{Timestamp: ts, Temp: map[int]float64{0: 79}})
		ts += 60
		rows = append(rows, CSVRow{Timestamp: ts, Temp: map[int]float64{0: 80}})
		ts += 60
		rows = append(rows, CSVRow{Timestamp: ts, Temp: map[int]float64{0: 81}})
		ts += 60
	}
	for i := 0; i < 10; i++ {
		rows = append(rows, CSVRow{Timestamp: ts, Temp: map[int]float64{0: 50}})
		ts += 60
	}
	return rows
}

func makeDetectionRows(v float64, n int) []CSVRow {
	var rows []CSVRow
	ts := int64(5_000_000)
	for i := 0; i < n; i++ {
		rows = append(rows, CSVRow{Timestamp: ts, Temp: map[int]float64{0: v}})
		ts += 60
	}
	return rows
}

func TestBuildBaselinesRobustAgainstPollution(t *testing.T) {
	baselines := BuildBaselines(buildTempBaselineRows(), []int{0})
	bl := baselines[0][MetricTemp]
	if bl == nil || bl.N < 2 {
		t.Fatalf("expected a real baseline, got nil or sparse")
	}
	// Median stays at the normal level (80) — not dragged by the 50°C pollution.
	if math.Abs(bl.Median-80) > 0.01 {
		t.Errorf("Median = %v, want ≈80 (robust center)", bl.Median)
	}
	// MAD stays small (≈1, the normal spread) — not inflated by the 10 fault points.
	if math.Abs(bl.Mad-1) > 0.01 {
		t.Errorf("Mad = %v, want ≈1", bl.Mad)
	}
	// Contrast: the classic std IS polluted (inflated well beyond the robust scale).
	if bl.StdDev <= madToStdFactor*bl.Mad {
		t.Errorf("StdDev=%v should exceed robust scale=%v; pollution inflates classic std",
			bl.StdDev, madToStdFactor*bl.Mad)
	}
}

// =============================================================================
// detectTimeAnomalies — MAD branch (temp/power/aicore_freq)
// =============================================================================

func TestDetectTimeAnomaliesMAD(t *testing.T) {
	cfg := DefaultDetectionConfig()
	baselines := BuildBaselines(buildTempBaselineRows(), []int{0})

	// Normal current window → no false positive despite the polluted baseline.
	res := detectTimeAnomalies(makeDetectionRows(80, 60), baselines, []int{0}, cfg)
	if z := res.Scores[0][MetricTemp]; z > 0.5 {
		t.Errorf("normal current → robust Z = %v, want ≈0 (no false positive)", z)
	}

	// Degraded current window → clearly anomalous (would be missed by a
	// mean/std baseline dragged toward 50°C).
	res = detectTimeAnomalies(makeDetectionRows(55, 60), baselines, []int{0}, cfg)
	if z := res.Scores[0][MetricTemp]; z <= cfg.TimeZThreshold {
		t.Errorf("degraded current → robust Z = %v, want > threshold %v", z, cfg.TimeZThreshold)
	}
}

// =============================================================================
// detectTimeAnomalies — classic mean/std branch unchanged (other metrics)
// =============================================================================

func TestDetectTimeAnomaliesClassicZScoreUnchanged(t *testing.T) {
	cfg := DefaultDetectionConfig()
	// TXBandwidth uses TimeMethod=MethodZScore → must keep the exact classic formula.
	var baselineRows []CSVRow
	ts := int64(1_000_000)
	for i := 0; i < 100; i++ {
		v := 100.0
		if i == 30 || i == 70 {
			v = 1000 // outliers still enter mean/std, as before
		}
		baselineRows = append(baselineRows, CSVRow{Timestamp: ts, TXBandwidth: map[int]float64{0: v}})
		ts += 60
	}
	baselines := BuildBaselines(baselineRows, []int{0})
	bl := baselines[0][MetricTXBandwidth]
	if bl == nil {
		t.Fatalf("expected TXBandwidth baseline")
	}

	var detRows []CSVRow
	for i := 0; i < 60; i++ {
		detRows = append(detRows, CSVRow{Timestamp: ts, TXBandwidth: map[int]float64{0: 110}})
		ts += 60
	}
	res := detectTimeAnomalies(detRows, baselines, []int{0}, cfg)
	z := res.Scores[0][MetricTXBandwidth]

	expected := math.Abs(110-bl.Mean) / bl.StdDev
	if math.Abs(z-expected) > 1e-6 {
		t.Errorf("classic Z = %v, want %v (mean=%v std=%v)", z, expected, bl.Mean, bl.StdDev)
	}
}

// =============================================================================
// detectTimeAnomalies — MAD branch for bimodal metrics (aicore_util, hbm_bandwidth_util)
// =============================================================================

// buildBimodalBaselineRows returns baseline rows for card 0's aicore_util:
// 80% work state (~88-94%) + 20% idle (~0-5%). This simulates a GPU that is
// mostly under load but has occasional idle minutes in the baseline window.
func buildBimodalBaselineRows() []CSVRow {
	var rows []CSVRow
	ts := int64(1_000_000)
	// 80 work points with small spread (88-94%).
	workPattern := []float64{88, 89, 90, 91, 92, 93, 94, 90, 91, 89}
	for i := 0; i < 80; i++ {
		v := workPattern[i%len(workPattern)]
		rows = append(rows, CSVRow{
			Timestamp:  ts,
			AICoreUtil: map[int]float64{0: v},
		})
		ts += 60
	}
	// 20 idle points (0-5%).
	idlePattern := []float64{0, 2, 4, 1, 3, 0, 5, 2, 1, 0}
	for i := 0; i < 20; i++ {
		v := idlePattern[i%len(idlePattern)]
		rows = append(rows, CSVRow{
			Timestamp:  ts,
			AICoreUtil: map[int]float64{0: v},
		})
		ts += 60
	}
	return rows
}

func TestBimodalMADImmuneToIdlePollution(t *testing.T) {
	baselines := BuildBaselines(buildBimodalBaselineRows(), []int{0})
	bl := baselines[0][MetricAICoreUtil]
	if bl == nil || bl.N < 2 {
		t.Fatalf("expected a real baseline, got nil or sparse")
	}

	// 1. Median stays in the work cluster (~90) — not dragged down by 20% idle.
	if math.Abs(bl.Median-90.5) > 2 {
		t.Errorf("Median = %v, want ≈90.5 (robust center in work cluster)", bl.Median)
	}
	// 2. MAD reflects work-cluster spread (~2), not the idle→90 deviations (~90).
	if bl.Mad > 5 {
		t.Errorf("Mad = %v, want ≤5 (work-cluster spread, not inflated by idle gap)", bl.Mad)
	}
	// 3. Contrast: classic mean IS dragged down by idle points.
	// 80×~90 + 20×~2 = ~7240, mean ≈ 72.4 — well below the work cluster.
	if bl.Mean > 80 {
		t.Errorf("Mean = %v, want <80 (dragged down by idle tail)", bl.Mean)
	}
	// 4. Classic std IS inflated by the gap between idle and work.
	if bl.StdDev <= madToStdFactor*bl.Mad {
		t.Errorf("StdDev=%v should exceed robust scale=%v; idle points inflate classic std",
			bl.StdDev, madToStdFactor*bl.Mad)
	}
}

func TestBimodalMADDetection(t *testing.T) {
	cfg := DefaultDetectionConfig()
	baselines := BuildBaselines(buildBimodalBaselineRows(), []int{0})

	// Normal detection window (all work state, ~90%) → no false positive.
	normalRows := makeAICoreUtilRows(90, 30)
	res := detectTimeAnomalies(normalRows, baselines, []int{0}, cfg)
	if z := res.Scores[0][MetricAICoreUtil]; z > 0.5 {
		t.Errorf("normal work window → robust Z = %v, want ≈0 (no false positive)", z)
	}

	// Degraded detection window (util dropped to 50%) → clearly anomalous.
	degradedRows := makeAICoreUtilRows(50, 30)
	res = detectTimeAnomalies(degradedRows, baselines, []int{0}, cfg)
	if z := res.Scores[0][MetricAICoreUtil]; z <= cfg.TimeZThreshold {
		t.Errorf("degraded window → robust Z = %v, want > threshold %v", z, cfg.TimeZThreshold)
	}
}

func makeAICoreUtilRows(v float64, n int) []CSVRow {
	var rows []CSVRow
	ts := int64(5_000_000)
	for i := 0; i < n; i++ {
		rows = append(rows, CSVRow{
			Timestamp:  ts,
			AICoreUtil: map[int]float64{0: v},
		})
		ts += 60
	}
	return rows
}
