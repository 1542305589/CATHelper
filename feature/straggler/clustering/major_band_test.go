package clustering

import (
	"math"
	"testing"
)

// The nearest baseline must use ratio distance |value/center - 1|, not absolute
// distance. With baselines 100 and 1000, 300 is closer to 1000 by ratio.
func TestNearestBaselineUsesRatioDistance(t *testing.T) {
	center, ratio := nearestBaseline(300, []float64{100, 1000})
	if center != 1000 {
		t.Fatalf("nearestBaseline(300) center = %v, want 1000 (ratio distance)", center)
	}
	if math.Abs(ratio-0.3) > 1e-9 {
		t.Fatalf("nearestBaseline(300) ratio = %v, want 0.3", ratio)
	}
}

// Values <= 0 are excluded before clustering and never produce a ratio-0
// anomaly; all non-positive input produces no baseline at all.
func TestDetectMajorBandFiltersNonPositive(t *testing.T) {
	baselines, anomalies := DetectMajorBand([]float64{0, -1, 100, 100, 100})
	if len(baselines) != 1 || len(anomalies) != 0 {
		t.Fatalf("non-positive filter = baselines %v anomalies %+v, want one baseline and no anomalies", baselines, anomalies)
	}
	if b, a := DetectMajorBand([]float64{0, -1, -2}); b != nil || a != nil {
		t.Fatalf("all non-positive must not detect: baselines=%v anomalies=%+v", b, a)
	}
}
