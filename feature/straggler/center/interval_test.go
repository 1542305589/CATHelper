package center

import (
	"net/http"
	"testing"
	"time"
)

func TestBusinessIntervalMinimum(t *testing.T) {
	c := newKPITestCenter(t)
	b := c.biz["biz"]
	b.Daemons[0].reportEtaMs = 156_000
	b.Daemons[0].reportEtaAt = time.Now()

	// eta 156s -> timeout 216s -> minimum interval 336s.
	rr := serveCenter(c, http.MethodPost, "/center/business/biz/interval", `{"interval_sec":300}`, nil)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("short interval status = %d, want 400 (body=%s)", rr.Code, rr.Body.String())
	}
	rr = serveCenter(c, http.MethodPost, "/center/business/biz/interval", `{"interval_sec":400}`, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("valid interval status = %d, want 200 (body=%s)", rr.Code, rr.Body.String())
	}
	if b.IntervalSec != 400 {
		t.Fatalf("business interval = %d, want 400", b.IntervalSec)
	}
}

func TestAddBusinessMinimumInterval(t *testing.T) {
	c := newKPITestCenter(t)
	rr := serveCenter(c, http.MethodPost, "/center/business",
		`{"name":"b2","interval_sec":300,"kpi_interval_sec":60}`, nil)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("short new-business interval status = %d, want 400 (body=%s)", rr.Code, rr.Body.String())
	}
	rr = serveCenter(c, http.MethodPost, "/center/business",
		`{"name":"b3","interval_sec":600,"kpi_interval_sec":60}`, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("valid new-business interval status = %d, want 200 (body=%s)", rr.Code, rr.Body.String())
	}
}
