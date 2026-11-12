package center

import (
	"math"
	"testing"
)

func TestKPIMedianMAD(t *testing.T) {
	median, mad := medianMAD([]float64{1, 2, 3, 4, 5})
	if median != 3 || mad != 1 {
		t.Fatalf("medianMAD = (%v, %v), want (3, 1)", median, mad)
	}
	if z := modifiedZ(10, 3, 1, 0.0001); math.Abs(z-4.7215) > 1e-3 {
		t.Errorf("modifiedZ = %v", z)
	}
}

func TestKPIFilterWorkingReference(t *testing.T) {
	ref := []kpiRefPoint{
		{Ts: 1, Metrics: map[string]float64{"aicore_freq": 1800}},
		{Ts: 2, Metrics: map[string]float64{"aicore_freq": 1800}},
		{Ts: 3, Metrics: map[string]float64{"aicore_freq": 1800}},
		{Ts: 4, Metrics: map[string]float64{"aicore_freq": 800}},
	}
	got := filterWorkingReference(ref, 1800)
	if len(got) != 3 {
		t.Fatalf("filtered len = %d, want 3: %+v", len(got), got)
	}
	for _, point := range got {
		if point.Metrics["aicore_freq"] != 1800 {
			t.Errorf("kept non-working point: %+v", point)
		}
	}
}

func TestKPIAggregateCurrent(t *testing.T) {
	samples := []kpiStoredSample{
		{Device: 0, Ts: 100, Metrics: map[string]float64{"power": 10, "aicore_freq": 1800}},
		{Device: 0, Ts: 101, Metrics: map[string]float64{"power": 20, "aicore_freq": 1800}},
		{Device: 1, Ts: 100, Metrics: map[string]float64{"power": 30}},
	}
	current := aggregateCurrent(samples, []int{0, 1})
	if len(current) != 2 {
		t.Fatalf("current = %+v", current)
	}
	if current[0]["power"] != 15 || current[0]["aicore_freq"] != 1800 {
		t.Errorf("device 0 = %+v", current[0])
	}
	if current[1]["power"] != 30 {
		t.Errorf("device 1 = %+v", current[1])
	}
}

func TestKPIDetectorSpaceAnomaly(t *testing.T) {
	c := New(Config{DataDir: t.TempDir(), Port: 1})
	defer func() { _ = c.kpi.Close() }()
	c.biz["biz"] = &Business{Name: "biz"}
	now := int64(1000)
	vals := map[string]float64{"power": 100, "aicore_freq": 1800}
	samples := []kpiSample{
		{Ts: now - 5, Vals: map[string]map[string]float64{
			"0": vals, "1": vals, "2": vals,
			"3": {"power": 1000, "aicore_freq": 1800},
		}},
	}
	if _, err := c.kpi.Insert("biz", "d1", "node-a", samples); err != nil {
		t.Fatal(err)
	}
	if _, err := c.kpiDet.runForBusiness("biz", defaultKPIDetectInterval, DefaultKPINSigma(), DefaultKPIReferenceAdmitRatio, nil); err != nil {
		t.Fatalf("runForBusiness: %v", err)
	}
	var count int
	if err := c.kpi.db.QueryRow(`SELECT COUNT(*) FROM kpi_anomaly WHERE business='biz' AND kind='space' AND device=3 AND metric='power'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("space anomaly count = %d, want 1", count)
	}
}

func TestKPIDetectorTimeAnomaly(t *testing.T) {
	c := New(Config{DataDir: t.TempDir(), Port: 1})
	defer func() { _ = c.kpi.Close() }()
	c.biz["biz"] = &Business{Name: "biz"}
	now := int64(10000)

	ref := make([]kpiSample, 0, kpiMinReferencePoints)
	for i := 0; i < kpiMinReferencePoints*10; i++ {
		ref = append(ref, kpiSample{
			Ts: now - 2000 + int64(i),
			Vals: map[string]map[string]float64{
				"0": {"power": 100, "aicore_freq": 1800},
			},
		})
	}
	if _, err := c.kpi.Insert("biz", "d1", "node-a", ref); err != nil {
		t.Fatal(err)
	}
	if _, err := c.kpi.Insert("biz", "d1", "node-a", []kpiSample{{
		Ts: now - 5,
		Vals: map[string]map[string]float64{
			"0": {"power": 1000, "aicore_freq": 1800},
			"1": {"power": 100, "aicore_freq": 1800},
		},
	}}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.kpiDet.runForBusiness("biz", defaultKPIDetectInterval, DefaultKPINSigma(), DefaultKPIReferenceAdmitRatio, nil); err != nil {
		t.Fatalf("runForBusiness: %v", err)
	}
	var count int
	if err := c.kpi.db.QueryRow(`SELECT COUNT(*) FROM kpi_anomaly WHERE business='biz' AND kind='time' AND device=0 AND metric='power'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("time anomaly count = %d, want 1", count)
	}
}

func TestKPIRecordAnomalyStreak(t *testing.T) {
	c := New(Config{DataDir: t.TempDir(), Port: 1})
	defer func() { _ = c.kpi.Close() }()
	b := &Business{Name: "biz", triggering: true}
	c.biz["biz"] = b

	c.recordKPIAnomaly("biz", true)
	c.recordKPIAnomaly("biz", true)
	if b.kpiAnomalyStreak != 2 {
		t.Fatalf("streak = %d, want 2", b.kpiAnomalyStreak)
	}
	c.recordKPIAnomaly("biz", false)
	if b.kpiAnomalyStreak != 0 {
		t.Fatalf("streak after normal = %d, want 0", b.kpiAnomalyStreak)
	}
}
