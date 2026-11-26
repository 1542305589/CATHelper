package daemon

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestReportEtaP90AndMinInterval(t *testing.T) {
	d := New(Config{}, nil)
	if got := d.reportEtaP90(); got != 0 {
		t.Fatalf("empty reportEtaP90 = %d, want 0", got)
	}
	d.reportEtas = []int64{100_000, 200_000}
	if got := d.reportEtaP90(); got != 200_000 {
		t.Fatalf("fewer than 3 samples must use max: got %d", got)
	}
	d.reportEtas = []int64{1000, 2000, 3000, 4000, 5000, 6000, 7000, 8000, 9000, 10000}
	if got := d.reportEtaP90(); got != 9000 {
		t.Fatalf("p90 of 10 samples = %d, want 9000", got)
	}
	// 300s ETA -> 300 + 130 = 430s minimum.
	d.reportEtas = []int64{300_000}
	if got := d.minIntervalSec(); got != 430 {
		t.Fatalf("minIntervalSec = %d, want 430", got)
	}
}

func TestSetIntervalMinimum(t *testing.T) {
	d := New(Config{Iterations: 3}, nil)
	d.reportEtas = []int64{300_000} // minimum 430s
	if err := d.SetInterval(400); err == nil {
		t.Fatal("SetInterval(400) below eta+130 must fail")
	}
	if err := d.SetInterval(430); err != nil {
		t.Fatalf("SetInterval(430): %v", err)
	}
	if got := int64(d.interval.Seconds()); got != 430 {
		t.Fatalf("interval = %d, want 430", got)
	}
}

func TestMatchRejectsShortInterval(t *testing.T) {
	d := New(Config{Iterations: 3}, nil)
	d.reportEtas = []int64{300_000} // minimum 430s
	if err := d.Match("http://center:8080", "secret", "biz", "n1", 400*time.Second); err == nil {
		t.Fatal("Match(400s) below eta+130 must fail")
	}
	if err := d.Match("http://center:8080", "secret", "biz", "n1", 600*time.Second); err != nil {
		t.Fatalf("Match(600s): %v", err)
	}
}

func TestUnmatchClampsInterval(t *testing.T) {
	d := New(Config{Iterations: 3}, nil)
	d.reportEtas = []int64{300_000} // minimum 430s
	d.state = "managed"
	d.centerInterval = 400 * time.Second
	d.interval = 400 * time.Second
	d.Unmatch()
	if d.interval < 430*time.Second {
		t.Fatalf("unmatched interval = %s, want >= 430s", d.interval)
	}
}

func TestStatusExposesReportEtaAndMinInterval(t *testing.T) {
	d := New(Config{Iterations: 3}, nil)
	d.reportEtas = []int64{156_000}
	rr := httptest.NewRecorder()
	d.handleStatus(rr, httptest.NewRequest(http.MethodGet, "/status", nil))
	var resp statusResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode /status: %v", err)
	}
	if resp.ReportEtaMs != 156_000 {
		t.Fatalf("report_eta_ms = %d, want 156000", resp.ReportEtaMs)
	}
	if resp.MinIntervalSec != 300 {
		t.Fatalf("min_interval_sec = %d, want 300", resp.MinIntervalSec)
	}
}
