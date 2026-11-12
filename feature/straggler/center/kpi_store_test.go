package center

import (
	"path/filepath"
	"testing"
	"time"
)

func testKPIStore(t *testing.T) *kpiStore {
	t.Helper()
	s, err := newKPIStore(filepath.Join(t.TempDir(), "kpi.db"), nil)
	if err != nil {
		t.Fatalf("newKPIStore: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func kpiSamples(ts int64, devices map[string]float64) kpiSample {
	vals := make(map[string]map[string]float64, len(devices))
	for device, temp := range devices {
		vals[device] = map[string]float64{"temp": temp, "hbm_util": temp + 1}
	}
	return kpiSample{Ts: ts, Vals: vals}
}

func TestKPIStoreLatestAndHistory(t *testing.T) {
	s := testKPIStore(t)
	if _, err := s.Insert("biz", "d1", "node-a", []kpiSample{
		kpiSamples(100, map[string]float64{"0": 40, "1": 41}),
		kpiSamples(101, map[string]float64{"0": 50, "1": 51}),
	}); err != nil {
		t.Fatalf("insert: %v", err)
	}

	latest, err := s.Latest("biz", "", nil)
	if err != nil {
		t.Fatalf("latest: %v", err)
	}
	if len(latest) != 2 {
		t.Fatalf("latest rows = %d, want 2", len(latest))
	}
	for _, row := range latest {
		if row.Ts != 101 {
			t.Errorf("latest device %d ts = %d, want 101", row.Device, row.Ts)
		}
		if row.Metrics["temp"] != 50 && row.Metrics["temp"] != 51 {
			t.Errorf("latest device %d metrics = %v", row.Device, row.Metrics)
		}
	}

	history, err := s.History("biz", 100, 101, "", nil, 100)
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	if len(history) != 4 {
		t.Fatalf("history rows = %d, want 4", len(history))
	}
	if history[0].Ts != 100 || history[len(history)-1].Ts != 101 {
		t.Errorf("history not oldest-first: first=%d last=%d", history[0].Ts, history[len(history)-1].Ts)
	}

	// Duplicate report is idempotent.
	if _, err := s.Insert("biz", "d1", "node-a", []kpiSample{
		kpiSamples(101, map[string]float64{"0": 55}),
	}); err != nil {
		t.Fatalf("duplicate insert: %v", err)
	}
	latest, _ = s.Latest("biz", "", nil)
	if len(latest) != 2 {
		t.Fatalf("duplicate insert changed row count: %d", len(latest))
	}
}

func TestKPIStoreFilter(t *testing.T) {
	s := testKPIStore(t)
	if _, err := s.Insert("biz", "d1", "node-a", []kpiSample{
		kpiSamples(100, map[string]float64{"0": 40, "1": 41, "2": 42}),
	}); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if err := s.SetFilter("biz", "node-a", []int{1}, false); err != nil {
		t.Fatalf("set filter: %v", err)
	}
	latest, err := s.Latest("biz", "", nil)
	if err != nil {
		t.Fatalf("latest: %v", err)
	}
	if len(latest) != 1 || latest[0].Device != 1 {
		t.Fatalf("filtered latest = %+v, want only device 1", latest)
	}
	filters, err := s.Filters("biz")
	if err != nil {
		t.Fatalf("filters: %v", err)
	}
	if len(filters) != 1 || filters[0].Node != "node-a" || len(filters[0].Devices) != 1 || filters[0].Devices[0] != 1 {
		t.Errorf("filters = %+v", filters)
	}

	// Empty devices removes the node filter → all cards allowed again.
	if err := s.SetFilter("biz", "node-a", nil, false); err != nil {
		t.Fatalf("clear filter: %v", err)
	}
	latest, _ = s.Latest("biz", "", nil)
	if len(latest) != 3 {
		t.Errorf("after clearing filter latest rows = %d, want 3", len(latest))
	}
}

func TestKPIStoreDeleteBusiness(t *testing.T) {
	s := testKPIStore(t)
	if _, err := s.Insert("biz", "d1", "node-a", []kpiSample{kpiSamples(100, map[string]float64{"0": 40})}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetFilter("biz", "node-a", []int{0}, false); err != nil {
		t.Fatal(err)
	}
	if err := s.InsertAnomalies([]kpiAnomaly{{
		Business: "biz", Node: "node-a", Device: 0, Ts: 100,
		Metric: "power", Kind: "time", Value: 1,
	}}); err != nil {
		t.Fatal(err)
	}
	if err := s.InsertDetectRun(kpiDetectRun{Business: "biz", Node: "node-a", Ts: 100}); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteBusiness("biz"); err != nil {
		t.Fatalf("delete business: %v", err)
	}
	if n, _ := s.AnomaliesAt("biz", 100); len(n) != 0 {
		t.Errorf("anomalies not deleted: %+v", n)
	}
	if runs, _ := s.DetectRuns("biz", 10); len(runs) != 0 {
		t.Errorf("runs not deleted: %+v", runs)
	}
	latest, err := s.Latest("biz", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(latest) != 0 {
		t.Errorf("latest after delete = %+v", latest)
	}
	filters, err := s.Filters("biz")
	if err != nil {
		t.Fatal(err)
	}
	if len(filters) != 0 {
		t.Errorf("filters after delete = %+v", filters)
	}
}

func TestKPIStoreDevicesIgnoreFilter(t *testing.T) {
	s := testKPIStore(t)
	if _, err := s.Insert("biz", "d1", "node-a", []kpiSample{
		kpiSamples(100, map[string]float64{"0": 40, "1": 41}),
		kpiSamples(101, map[string]float64{"0": 42}),
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetFilter("biz", "node-a", []int{0}, false); err != nil {
		t.Fatal(err)
	}
	nodes, err := s.Devices("biz")
	if err != nil {
		t.Fatalf("devices: %v", err)
	}
	if len(nodes) != 1 || nodes[0].Node != "node-a" || len(nodes[0].Devices) != 2 {
		t.Fatalf("devices = %+v, want node-a with devices 0,1", nodes)
	}
}

func TestKPIStoreAnnotatesLegacyNodes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "kpi.db")
	s1, err := newKPIStore(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s1.Insert("biz", "1.2.3.4:8080", "master", []kpiSample{
		kpiSamples(100, map[string]float64{"0": 40}),
	}); err != nil {
		t.Fatal(err)
	}
	// Simulate a pre-migration DB: no marker yet.
	if _, err := s1.db.Exec(`DELETE FROM kpi_meta WHERE key = 'node_annotated_v1'`); err != nil {
		t.Fatal(err)
	}
	_ = s1.Close()

	s2, err := newKPIStore(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	nodes, err := s2.Devices("biz")
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes) != 1 || nodes[0].Node != "master(1.2.3.4:8080)" {
		t.Errorf("nodes = %+v, want annotated legacy node", nodes)
	}
}

func TestKPIStoreFilterAll(t *testing.T) {
	s := testKPIStore(t)
	if _, err := s.Insert("biz", "d1", "node-a", []kpiSample{
		kpiSamples(100, map[string]float64{"0": 40, "1": 41}),
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetFilter("biz", "node-a", nil, true); err != nil {
		t.Fatalf("set filter all: %v", err)
	}
	latest, err := s.Latest("biz", "", nil)
	if err != nil {
		t.Fatalf("latest: %v", err)
	}
	if len(latest) != 0 {
		t.Errorf("filter-all latest = %+v, want 0 rows", latest)
	}
	history, err := s.History("biz", 100, 100, "", nil, 100)
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	if len(history) != 0 {
		t.Errorf("filter-all history = %+v, want 0 rows", history)
	}
	filters, err := s.Filters("biz")
	if err != nil {
		t.Fatalf("filters: %v", err)
	}
	if len(filters) != 1 || !filters[0].FilterAll || filters[0].Node != "node-a" {
		t.Errorf("filters = %+v, want filter_all node-a", filters)
	}
	devices, err := s.Devices("biz")
	if err != nil {
		t.Fatalf("devices: %v", err)
	}
	if len(devices) != 1 || len(devices[0].Devices) != 2 {
		t.Errorf("devices must ignore filter-all, got %+v", devices)
	}
	if err := s.SetFilter("biz", "node-a", nil, false); err != nil {
		t.Fatal(err)
	}
	latest, _ = s.Latest("biz", "", nil)
	if len(latest) != 2 {
		t.Errorf("after clear latest rows = %d, want 2", len(latest))
	}
}

func TestKPIStoreReferenceSamplesExcludeAnomalyWindow(t *testing.T) {
	s := testKPIStore(t)
	if _, err := s.Insert("biz", "d1", "node-a", []kpiSample{
		{Ts: 100, Vals: map[string]map[string]float64{"0": {"power": 100}}},
		{Ts: 101, Vals: map[string]map[string]float64{"0": {"power": 101}}},
		{Ts: 102, Vals: map[string]map[string]float64{"0": {"power": 102}}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.InsertAnomalies([]kpiAnomaly{{
		Business: "biz", Node: "node-a", Device: 0, Ts: 101,
		Metric: "power", Kind: "time", Value: 101,
	}}); err != nil {
		t.Fatal(err)
	}
	ref, err := s.ReferenceSamples("biz", "node-a", 0, 0, 200, 30, 10)
	if err != nil {
		t.Fatalf("ReferenceSamples: %v", err)
	}
	if len(ref) != 1 || ref[0].Ts != 102 {
		t.Fatalf("reference = %+v, want only ts 102", ref)
	}
}

func TestKPIStoreRecentSamples(t *testing.T) {
	s := testKPIStore(t)
	if _, err := s.Insert("biz", "d1", "node-a", []kpiSample{
		{Ts: 100, Vals: map[string]map[string]float64{"0": {"power": 100}}},
		{Ts: 101, Vals: map[string]map[string]float64{"0": {"power": 101}}},
	}); err != nil {
		t.Fatal(err)
	}
	samples, err := s.RecentSamples("biz", "node-a", 101)
	if err != nil {
		t.Fatalf("RecentSamples: %v", err)
	}
	if len(samples) != 1 || samples[0].Ts != 101 || samples[0].Device != 0 {
		t.Fatalf("samples = %+v", samples)
	}
}

func TestKPIStoreDetectRunsAndRecentAnomalies(t *testing.T) {
	s := testKPIStore(t)
	if err := s.InsertDetectRun(kpiDetectRun{
		Business: "biz", Node: "node-a", Ts: 100, Devices: 8, Anomalies: 2, Flagged: 2, ReferenceInvalid: true,
	}); err != nil {
		t.Fatal(err)
	}
	runs, err := s.DetectRuns("biz", 10)
	if err != nil {
		t.Fatalf("DetectRuns: %v", err)
	}
	if len(runs) != 1 || runs[0].Devices != 8 || !runs[0].ReferenceInvalid {
		t.Fatalf("runs = %+v", runs)
	}
	if err := s.InsertAnomalies([]kpiAnomaly{{
		Business: "biz", Node: "node-a", Device: 3, Ts: 100,
		Metric: "power", Kind: "space", Value: 1000, Median: 100, Mad: 1, Z: 10,
	}}); err != nil {
		t.Fatal(err)
	}
	anomalies, err := s.RecentAnomalies("biz", 10)
	if err != nil {
		t.Fatalf("RecentAnomalies: %v", err)
	}
	if len(anomalies) != 1 || anomalies[0].Device != 3 || anomalies[0].Metric != "power" {
		t.Fatalf("anomalies = %+v", anomalies)
	}
}

func TestKPIStorePruneKPI(t *testing.T) {
	s := testKPIStore(t)
	now := time.Now().Unix()
	oldTs := now - 49*3600
	newTs := now - 3600
	if _, err := s.Insert("biz", "d1", "node-a", []kpiSample{
		kpiSamples(oldTs, map[string]float64{"0": 40}),
		kpiSamples(newTs, map[string]float64{"0": 41}),
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.InsertAnomalies([]kpiAnomaly{{Business: "biz", Node: "node-a", Device: 0, Ts: oldTs, Metric: "power", Kind: "time"}}); err != nil {
		t.Fatal(err)
	}
	if err := s.InsertDetectRun(kpiDetectRun{Business: "biz", Node: "node-a", Ts: oldTs}); err != nil {
		t.Fatal(err)
	}
	n, err := s.PruneKPI(48 * time.Hour)
	if err != nil {
		t.Fatalf("PruneKPI: %v", err)
	}
	if n == 0 {
		t.Fatal("expected deleted rows")
	}
	rows, err := s.RecentSamples("biz", "node-a", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Ts != newTs {
		t.Errorf("samples after prune = %+v, want only new ts", rows)
	}
}
