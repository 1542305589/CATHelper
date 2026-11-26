package daemon

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestReportCycleFailurePayloadAndRetry(t *testing.T) {
	var mu sync.Mutex
	attempts := 0
	var payload map[string]any
	var key string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		attempts++
		key = r.Header.Get("X-Match-Key")
		_ = json.NewDecoder(r.Body).Decode(&payload)
		if attempts == 1 {
			http.Error(w, "try again", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	d := New(Config{}, nil)
	d.state = "managed"
	d.centerAddr = srv.URL
	d.matchKey = "secret"
	d.business = "biz"
	d.daemonID = "n1"

	cr := &CycleResult{ID: 1, StartedAt: time.Now().Add(-time.Second), RoundToken: "round-1"}
	d.reportCycle(cr, "failed", "no_dump_after_collect", "collect")

	mu.Lock()
	defer mu.Unlock()
	if attempts != 2 {
		t.Fatalf("attempts = %d, want 2 (initial + one retry)", attempts)
	}
	if key != "secret" {
		t.Fatalf("X-Match-Key = %q, want secret", key)
	}
	if payload["round_token"] != "round-1" || payload["status"] != "failed" {
		t.Fatalf("payload = %+v", payload)
	}
	if payload["reason"] != "no_dump_after_collect" || payload["stage"] != "collect" {
		t.Fatalf("payload reason/stage = %v/%v", payload["reason"], payload["stage"])
	}
	if ranks, ok := payload["ranks"]; !ok || ranks != nil {
		t.Fatalf("failed report ranks = %v, want null", payload["ranks"])
	}
	if cr.ReportStatus != "failed" || cr.ReportReason != "no_dump_after_collect" || cr.ReportStage != "collect" {
		t.Fatalf("cycle report fields = %+v", cr)
	}
}

func TestReportCycleOKPayload(t *testing.T) {
	archive := t.TempDir()
	opDir := filepath.Join(archive, "op_metric")
	if err := os.MkdirAll(opDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(opDir, "global_rank_0.csv"),
		[]byte("StepIndex,ZP_Kernel\n0,100\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&payload)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	d := New(Config{}, nil)
	d.state = "managed"
	d.centerAddr = srv.URL
	d.matchKey = "secret"
	d.business = "biz"
	d.daemonID = "n1"
	cr := &CycleResult{ID: 1, StartedAt: time.Now().Add(-time.Second), DumpDir: archive, RoundToken: "round-1"}
	d.reportCycle(cr, "ok", "", "")

	if cr.ReportStatus != "ok" || cr.ReportEtaMs <= 0 {
		t.Fatalf("cycle report fields = status=%q eta=%d", cr.ReportStatus, cr.ReportEtaMs)
	}
	if payload["status"] != "ok" {
		t.Fatalf("payload status = %v", payload["status"])
	}
	ranks, ok := payload["ranks"].(map[string]any)
	if !ok || len(ranks) != 1 {
		t.Fatalf("payload ranks = %+v", payload["ranks"])
	}
}

func TestReportCycleSkippedWhenUnmanaged(t *testing.T) {
	requests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
	}))
	defer srv.Close()

	d := New(Config{}, nil)
	d.state = "running"
	d.centerAddr = srv.URL
	d.business = "biz"
	d.daemonID = "n1"

	cr := &CycleResult{ID: 1, StartedAt: time.Now(), RoundToken: "round-1"}
	d.reportCycle(cr, "failed", "parse_failed", "parse")

	if requests != 0 {
		t.Fatalf("unmanaged daemon sent %d reports, want 0", requests)
	}
	if cr.ReportStatus != "skipped" {
		t.Fatalf("ReportStatus = %q, want skipped", cr.ReportStatus)
	}
}

func TestDynoFailureReason(t *testing.T) {
	if got := dynoFailureReason(nil); got != "dyno_trigger_failed" {
		t.Fatalf("nil reason = %q", got)
	}
	if got := dynoFailureReason(os.ErrNotExist); got != "dyno_trigger_failed" {
		t.Fatalf("generic reason = %q", got)
	}
	if got := dynoFailureReason(errString("dyno processesMatched empty")); got != "no_target_process" {
		t.Fatalf("no process reason = %q", got)
	}
}

func TestHasMasterDump(t *testing.T) {
	dir := t.TempDir()
	if hasMasterDump(dir) {
		t.Fatal("empty dir must not have a master dump")
	}
	if err := os.Mkdir(filepath.Join(dir, "master_1_2_ascend_pt"), 0o755); err != nil {
		t.Fatal(err)
	}
	if !hasMasterDump(dir) {
		t.Fatal("master_* dir must be detected")
	}
}

type errString string

func (e errString) Error() string { return string(e) }
