package daemon

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestSetIterationsRuntime(t *testing.T) {
	d := New(Config{Iterations: 3}, nil)
	if got := d.Iterations(); got != 3 {
		t.Fatalf("initial iterations = %d, want 3", got)
	}
	if err := d.SetIterations(5); err != nil {
		t.Fatalf("SetIterations(5): %v", err)
	}
	if got := d.Iterations(); got != 5 {
		t.Fatalf("iterations = %d, want 5", got)
	}
	for _, bad := range []int{0, -1, 101} {
		if err := d.SetIterations(bad); err == nil {
			t.Fatalf("SetIterations(%d) = nil, want error", bad)
		}
	}
	if got := d.Iterations(); got != 5 {
		t.Fatalf("iterations changed after invalid input: %d", got)
	}
	// Below 3 needs the daemon's startup debug switch.
	if err := d.SetIterations(2); err == nil {
		t.Fatalf("SetIterations(2) without --allow-low-iterations = nil, want error")
	}
	low := New(Config{Iterations: 3, AllowLowIterations: true}, nil)
	if err := low.SetIterations(2); err != nil {
		t.Fatalf("SetIterations(2) with --allow-low-iterations: %v", err)
	}
	if got := low.Iterations(); got != 2 {
		t.Fatalf("low iterations = %d, want 2", got)
	}
}

func TestNewDefaultsIterations(t *testing.T) {
	d := New(Config{}, nil)
	if got := d.Iterations(); got != 3 {
		t.Fatalf("default iterations = %d, want 3", got)
	}
}

func TestIterationsManagedLocalEditAllowedForSelfRun(t *testing.T) {
	d := New(Config{Iterations: 3}, nil)
	d.state = "managed"
	d.matchKey = "secret"

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/daemon/iterations", strings.NewReader(`{"iterations":5}`))
	d.handleDaemonSetIterations(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("local edit while managed status = %d, want 200 (body=%s)", rr.Code, rr.Body.String())
	}
	if got := d.Iterations(); got != 5 {
		t.Fatalf("saved self-run iterations = %d, want 5", got)
	}
}

func TestIterationsUnmanagedLocalEditAllowed(t *testing.T) {
	d := New(Config{Iterations: 3}, nil)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/daemon/iterations", strings.NewReader(`{"iterations":7}`))
	d.handleDaemonSetIterations(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("unmanaged edit status = %d, want 200 (body=%s)", rr.Code, rr.Body.String())
	}
	if got := d.Iterations(); got != 7 {
		t.Fatalf("iterations = %d, want 7", got)
	}
}

func TestMatchKeepsLocalIterations(t *testing.T) {
	d := New(Config{Iterations: 5}, nil)
	if err := d.Match("http://center:8080", "secret", "biz", "n1", 10*time.Minute); err != nil {
		t.Fatalf("Match: %v", err)
	}
	if got := d.Iterations(); got != 5 {
		t.Fatalf("match must not overwrite the saved self-run value: got %d, want 5", got)
	}
}

// /daemon/trigger must carry an explicit per-cycle iteration count.
func TestTriggerRejectsMissingOrInvalidIterations(t *testing.T) {
	for _, body := range []string{
		`{"round_token":"t"}`,
		`{"iterations":0}`,
		`{"iterations":-1}`,
		`{"iterations":101}`,
		`{"iterations":2}`, // <3 without --allow-low-iterations
	} {
		d := New(Config{Iterations: 3}, nil)
		rr := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/daemon/trigger", strings.NewReader(body))
		d.handleDaemonTrigger(rr, req)
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("body %s status = %d, want 400 (resp=%s)", body, rr.Code, rr.Body.String())
		}
		d.mu.Lock()
		started := d.cycleInFlight
		d.mu.Unlock()
		if started {
			t.Fatalf("body %s started a cycle, want rejection", body)
		}
	}
}

func TestCycleSummaryIncludesIterations(t *testing.T) {
	summary := toCycleSummary(&CycleResult{ID: 1, Iterations: 7})
	if summary == nil || summary.Iterations != 7 {
		t.Fatalf("cycle summary iterations = %+v, want 7", summary)
	}
}

func TestStatusIncludesRuntimeIterations(t *testing.T) {
	d := New(Config{Iterations: 3}, nil)
	if err := d.SetIterations(6); err != nil {
		t.Fatalf("SetIterations: %v", err)
	}
	rr := httptest.NewRecorder()
	d.handleStatus(rr, httptest.NewRequest(http.MethodGet, "/status", nil))
	var resp statusResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode /status: %v", err)
	}
	if resp.Iterations != 6 {
		t.Fatalf("/status iterations = %d, want 6", resp.Iterations)
	}
}
