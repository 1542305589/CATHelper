package center

import (
	"math"
	"testing"
)

func bandValues(vals ...float64) map[int]float64 {
	out := make(map[int]float64, len(vals))
	for i, v := range vals {
		out[i] = v
	}
	return out
}

func TestDetectMajorBandNormal(t *testing.T) {
	baselines, anomalies := DetectMajorBand(bandValues(
		98, 99, 100, 100, 100, 100, 101, 101,
		102, 100, 100, 99, 100, 100, 100, 100,
	))
	if len(baselines) != 1 {
		t.Fatalf("baselines = %v, want 1 cluster", baselines)
	}
	if math.Abs(baselines[0]-100) > 2 {
		t.Fatalf("baseline = %v, want ~100", baselines[0])
	}
	if len(anomalies) != 0 {
		t.Fatalf("anomalies = %+v, want none", anomalies)
	}
}

func TestDetectMajorBandTiedBaselines(t *testing.T) {
	baselines, anomalies := DetectMajorBand(bandValues(
		100, 100, 100, 100, 200, 200, 200, 200,
	))
	if len(baselines) != 2 {
		t.Fatalf("baselines = %v, want 2 tied clusters", baselines)
	}
	if baselines[0] != 100 || baselines[1] != 200 {
		t.Fatalf("baselines = %v, want [100 200]", baselines)
	}
	if len(anomalies) != 0 {
		t.Fatalf("anomalies = %+v, want none (both are baselines)", anomalies)
	}
}

func TestDetectMajorBandOutOfBand(t *testing.T) {
	// 130 is exactly 1.3x and 70 exactly 0.7x -> both still normal (closed).
	if _, anomalies := DetectMajorBand(bandValues(100, 100, 100, 100, 100, 130)); len(anomalies) != 0 {
		t.Fatalf("130 should be inside the 1.3 band: %+v", anomalies)
	}
	if _, anomalies := DetectMajorBand(bandValues(100, 100, 100, 100, 100, 70)); len(anomalies) != 0 {
		t.Fatalf("70 should be inside the 0.7 band: %+v", anomalies)
	}
	// 60 is 0.6x the baseline -> anomaly.
	_, low := DetectMajorBand(bandValues(100, 100, 100, 100, 100, 60))
	if len(low) != 1 {
		t.Fatalf("low anomalies = %+v, want 1", low)
	}
	if low[0].Device != 5 || low[0].Center != 100 || math.Abs(low[0].Ratio-0.6) > 1e-9 {
		t.Fatalf("low anomaly = %+v, want device=5 center=100 ratio=0.6", low[0])
	}
	// 140 is 1.4x the baseline -> anomaly.
	_, high := DetectMajorBand(bandValues(100, 100, 100, 100, 100, 140))
	if len(high) != 1 {
		t.Fatalf("high anomalies = %+v, want 1", high)
	}
	if high[0].Device != 5 || high[0].Center != 100 || math.Abs(high[0].Ratio-1.4) > 1e-9 {
		t.Fatalf("high anomaly = %+v, want device=5 center=100 ratio=1.4", high[0])
	}
}

// Non-positive device values are excluded before clustering and never reported.
func TestDetectMajorBandFiltersNonPositiveDevices(t *testing.T) {
	if _, anomalies := DetectMajorBand(bandValues(0, -1, 100, 100, 100)); len(anomalies) != 0 {
		t.Fatalf("non-positive devices must not be anomalies: %+v", anomalies)
	}
	if baselines, anomalies := DetectMajorBand(bandValues(0, -1, -2)); baselines != nil || anomalies != nil {
		t.Fatalf("all non-positive must not detect: baselines=%v anomalies=%+v", baselines, anomalies)
	}
}

// Two tied baseline clusters plus one value near each outside edge: the ratio
// must use the NEAREST baseline cluster center.
func TestDetectMajorBandNearestBaseline(t *testing.T) {
	_, anomalies := DetectMajorBand(bandValues(
		65, 100, 100, 100, 100, 200, 200, 200, 200, 300,
	))
	if len(anomalies) != 2 {
		t.Fatalf("anomalies = %+v, want 2", anomalies)
	}
	byDevice := make(map[int]kpiBandAnomaly, len(anomalies))
	for _, a := range anomalies {
		byDevice[a.Device] = a
	}
	low, ok := byDevice[0]
	if !ok {
		t.Fatalf("device 0 anomaly missing: %+v", anomalies)
	}
	if low.Center != 93 || math.Abs(low.Ratio-65.0/93.0) > 1e-9 {
		t.Fatalf("device 0 = %+v, want nearest center 93 ratio 65/93", low)
	}
	high, ok := byDevice[9]
	if !ok {
		t.Fatalf("device 9 anomaly missing: %+v", anomalies)
	}
	if high.Center != 220 || math.Abs(high.Ratio-300.0/220.0) > 1e-9 {
		t.Fatalf("device 9 = %+v, want nearest center 220 ratio 300/220", high)
	}
}

func TestDetectMajorBandFewerThanThreeDevices(t *testing.T) {
	baselines, anomalies := DetectMajorBand(bandValues(100, 200))
	if baselines != nil || anomalies != nil {
		t.Fatalf("fewer than 3 devices must not detect: baselines=%v anomalies=%v", baselines, anomalies)
	}
}
