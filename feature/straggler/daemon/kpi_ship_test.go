package daemon

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeKPIAppend(t *testing.T, path, content string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(content); err != nil {
		t.Fatal(err)
	}
}

func TestKPITailerSkipsCPUAndPartialLines(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "straggler_kpi_2026-11-05.jsonl")
	if err := os.WriteFile(path, []byte(`{"ts":1,"vals":{"0":{"temp":40}}}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	tailer := &kpiTailer{dir: dir}
	if samples, err := tailer.readNew(); err != nil || len(samples) != 0 {
		t.Fatalf("initial read = %v, %v; want 0 samples (anchor at EOF)", samples, err)
	}

	writeKPIAppend(t, path,
		`{"ts":2,"vals":{"0":{"temp":41}}}`+"\n"+
			`{"ts":3,"cpu_avg":{"0":"0"}}`+"\n"+
			`{"ts":4,"vals":{"0":{"temp":42`)
	samples, err := tailer.readNew()
	if err != nil {
		t.Fatalf("readNew: %v", err)
	}
	if len(samples) != 1 || samples[0].Ts != 2 || samples[0].Vals["0"]["temp"] != 41 {
		t.Fatalf("samples = %+v, want only NPU ts=2", samples)
	}

	// Complete the partial line; it should be consumed on the next tick.
	writeKPIAppend(t, path, `}}}`+"\n")
	samples, err = tailer.readNew()
	if err != nil {
		t.Fatalf("readNew after partial completion: %v", err)
	}
	if len(samples) != 1 || samples[0].Ts != 4 {
		t.Fatalf("samples = %+v, want NPU ts=4", samples)
	}
}

func TestKPITailerSwitchesFileAndHandlesTruncate(t *testing.T) {
	dir := t.TempDir()
	oldPath := filepath.Join(dir, "straggler_kpi_2026-11-05.jsonl")
	if err := os.WriteFile(oldPath, []byte(`{"ts":1,"vals":{"0":{"temp":40}}}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	tailer := &kpiTailer{dir: dir}
	if _, err := tailer.readNew(); err != nil {
		t.Fatal(err)
	}

	newPath := filepath.Join(dir, "straggler_kpi_2026-11-06.jsonl")
	if err := os.WriteFile(newPath, []byte(`{"ts":2,"vals":{"0":{"temp":41}}}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	_ = os.Chtimes(oldPath, now.Add(-time.Hour), now.Add(-time.Hour))
	_ = os.Chtimes(newPath, now, now)
	samples, err := tailer.readNew()
	if err != nil {
		t.Fatalf("readNew new file: %v", err)
	}
	if len(samples) != 1 || samples[0].Ts != 2 {
		t.Fatalf("new file samples = %+v, want ts=2", samples)
	}

	// Truncate in place, observe the reset, then append a fresh line.
	if err := os.Truncate(newPath, 0); err != nil {
		t.Fatal(err)
	}
	if samples, err := tailer.readNew(); err != nil || len(samples) != 0 {
		t.Fatalf("readNew after truncate = %+v, %v; want 0 samples", samples, err)
	}
	writeKPIAppend(t, newPath, `{"ts":3,"vals":{"0":{"temp":42}}}`+"\n")
	samples, err = tailer.readNew()
	if err != nil {
		t.Fatalf("readNew after truncate append: %v", err)
	}
	if len(samples) != 1 || samples[0].Ts != 3 {
		t.Fatalf("truncate samples = %+v, want ts=3", samples)
	}
}

func TestKPIShipperPostsNPUSamples(t *testing.T) {
	var gotPath, gotKey string
	var got kpiShipReport
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotKey = r.Header.Get("X-Match-Key")
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("decode report: %v", err)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	d := New(Config{}, nil)
	s := &kpiShipper{d: d}
	err := s.post(srv.URL, "secret", "biz", "1.2.3.4:8080", []kpiShipSample{{
		Ts:   100,
		Vals: map[string]map[string]float64{"0": {"temp": 49}},
	}})
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	if gotPath != "/center/kpi/biz/1.2.3.4:8080" {
		t.Errorf("path = %q", gotPath)
	}
	if gotKey != "secret" {
		t.Errorf("key = %q", gotKey)
	}
	if len(got.Samples) != 1 || got.Samples[0].Ts != 100 || got.Samples[0].Vals["0"]["temp"] != 49 {
		t.Errorf("report = %+v", got)
	}
}
