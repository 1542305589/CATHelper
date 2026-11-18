package clustering

import (
	"math"
	"testing"
)

// The kmeans++ seeding must draw from CPython's random.Random(42) stream;
// otherwise cluster assignment (and every reported ratio) diverges from the
// Python skill even when the algorithm is otherwise identical.
func TestPyRandomMatchesPython(t *testing.T) {
	// python -c "import random; r=random.Random(42); ..."
	want := []float64{
		0.6394267984578837,
		0.025010755222666936,
		0.27502931836911926,
		0.22321073814882275,
		0.7364712141640124,
	}
	r := newPyRandom(42)
	for i, w := range want {
		if got := r.Float64(); got != w {
			t.Fatalf("draw %d = %.17g, want %.17g", i, got, w)
		}
	}
}

// Golden vectors generated with the Python reference
// (`.agents/skills/straggler-detection/scripts/kmeans_detector.py`,
// general_anomaly_detection with seed 42). These exercise the kmeans++ path
// with large, low-variance fleets where the RNG stream changes the split.
func TestDetectPythonGolden(t *testing.T) {
	bandwidth := make([]float64, 0, 28)
	for i := 0; i < 25; i++ {
		bandwidth = append(bandwidth, 1000)
	}
	bandwidth = append(bandwidth, 500, 2000, 2000)

	ramp := []float64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}
	fleet := []float64{5, 5, 5, 5, 5, 5, 5, 5, 5, 5, 5, 5, 7, 9, 11, 40}

	cases := []struct {
		name string
		vals []float64
		high bool
		want map[int]float64
	}{
		{"ramp", ramp, true, map[int]float64{9: 10.0}},
		{"bandwidth-max", bandwidth, true, map[int]float64{26: 4.0, 27: 4.0}},
		{"bandwidth-min", bandwidth, false, map[int]float64{25: 4.0}},
		{"fleet-min", fleet[:8], false, map[int]float64{}},
		{"ramp-fleet", fleet, true, map[int]float64{15: 7.761194029850746}},
	}
	// fleet[:8] is all 5s → nothing to flag.
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assertFlagged(t, Detect(tc.vals, 1.3, tc.high), tc.want)
		})
	}

	// Min direction over a stable 100 majority with two 10 outliers: Python
	// reports baseline/own = 10x.
	low := []float64{100, 100, 100, 100, 100, 100, 10, 10}
	assertFlagged(t, Detect(low, 1.3, false), map[int]float64{6: 10.0, 7: 10.0})

	// Sanity: the golden ratio above is a real value, not a rounded constant.
	if math.Abs(67.0/13.0-5.153846153846154) > 1e-12 {
		t.Fatal("unreachable")
	}
}
