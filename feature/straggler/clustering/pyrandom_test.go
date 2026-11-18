package clustering

import (
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

// Seeds whose bit length is a multiple of 32 must not gain a padding word
// (CPython keymax = ceil(bits/32)).
func TestPyRandomSeedEdgeCases(t *testing.T) {
	// python -c "import random; print(repr(random.Random(2147483648).random()))"
	r := newPyRandom(2147483648)
	if got, want := r.Float64(), 0.3387958510019964; got != want {
		t.Fatalf("seed 2^31 first draw = %.17g, want %.17g", got, want)
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

	// Duplicate-heavy integer vectors: CPython 3.12's compensated sum() changes
	// the kmeans split here, so naive summation diverges by ~1 ulp.
	dupA := []float64{1000, 1100, 2000, 1200, 1100, 1200, 10000, 1000, 1000, 1200,
		1100, 2000, 1000, 1200, 1000, 1500, 1000, 1100, 1000}
	assertFlagged(t, Detect(dupA, 2.5, true), map[int]float64{6: 10.0})

	dupB := []float64{15, 12, 30, 10, 12, 10, 30, 30, 10, 12, 15, 5, 15, 10, 5,
		11, 10, 11, 12, 20, 20, 30, 12, 100, 11, 10}
	assertFlagged(t, Detect(dupB, 2.5, false), map[int]float64{11: 20.0, 14: 20.0})

	// Min direction over a stable 100 majority with two 10 outliers: Python
	// reports baseline/own = 10x.
	low := []float64{100, 100, 100, 100, 100, 100, 10, 10}
	assertFlagged(t, Detect(low, 1.3, false), map[int]float64{6: 10.0, 7: 10.0})
}
