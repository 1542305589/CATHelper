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

func TestIterationsManagedLocalEditRejected(t *testing.T) {
	d := New(Config{Iterations: 3}, nil)
	d.state = "managed"
	d.matchKey = "secret"

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/daemon/iterations", strings.NewReader(`{"iterations":5}`))
	d.handleDaemonSetIterations(rr, req)
	if rr.Code != http.StatusConflict {
		t.Fatalf("local edit while managed status = %d, want 409 (body=%s)", rr.Code, rr.Body.String())
	}
	if got := d.Iterations(); got != 3 {
		t.Fatalf("iterations = %d after rejected edit, want 3", got)
	}

	// The matched center (correct key) may still change it.
	rr = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/daemon/iterations", strings.NewReader(`{"iterations":5}`))
	req.Header.Set("X-Match-Key", "secret")
	d.handleDaemonSetIterations(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("center edit status = %d, want 200 (body=%s)", rr.Code, rr.Body.String())
	}
	if got := d.Iterations(); got != 5 {
		t.Fatalf("iterations = %d after center edit, want 5", got)
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

func TestMatchAppliesCenterIterations(t *testing.T) {
	d := New(Config{Iterations: 3}, nil)
	if err := d.Match("http://center:8080", "secret", "biz", "n1", 10*time.Minute, 6); err != nil {
		t.Fatalf("Match: %v", err)
	}
	if got := d.Iterations(); got != 6 {
		t.Fatalf("iterations after match = %d, want 6", got)
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
