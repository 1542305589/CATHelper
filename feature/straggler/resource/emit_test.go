package resource

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
)

func TestEmitToFaultSubConfirmsCritical(t *testing.T) {
	var (
		mu     sync.Mutex
		events []FaultEvent
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/faultsub/events" || r.Method != http.MethodPost {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			http.Error(w, "bad", http.StatusBadRequest)
			return
		}
		body, _ := io.ReadAll(r.Body)
		var ev FaultEvent
		if err := json.Unmarshal(body, &ev); err != nil {
			t.Errorf("bad json: %v", err)
		}
		mu.Lock()
		events = append(events, ev)
		mu.Unlock()
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()

	result := &DetectionResult{
		Results: []CardDetectionSummary{
			{CardID: 3, Quadrant: QuadConfirmedAnomaly, AnomalyCategory: CatCompute, CompositeScore: 8.7},
			{CardID: 1, Quadrant: QuadEarlyDegradation, AnomalyCategory: CatCompute, CompositeScore: 5.1},
			{CardID: 2, Quadrant: QuadNormal, AnomalyCategory: CatNone, CompositeScore: 0}, // skipped
			{CardID: 7, Quadrant: QuadIndividualVariance, AnomalyCategory: CatCommunication, CompositeScore: 3.2},
		},
	}
	EmitToFaultSub(result, EmitConfig{URL: srv.URL, Timeout: 0})

	mu.Lock()
	defer mu.Unlock()
	if len(events) != 3 {
		t.Fatalf("expected 3 events (3 anomalous, 1 normal skipped), got %d", len(events))
	}
	// Card 3 → critical (node defaults to "none").
	ev3 := findEmit(events, "none:3")
	if ev3 == nil {
		t.Fatal("missing card 3 event")
	}
	if ev3.Type != "straggler_detected" || ev3.Severity != "critical" {
		t.Errorf("card3 event wrong: %+v", ev3)
	}
	if ev3.Detail["quadrant"] != "confirmed_anomaly" {
		t.Errorf("card3 detail wrong: %+v", ev3.Detail)
	}
	if ev3.Detail["composite_score"] != strconv.FormatFloat(8.7, 'f', -1, 64) {
		t.Errorf("composite_score wrong: %+v", ev3.Detail)
	}
	// Card 1 → warning (early degradation).
	ev1 := findEmit(events, "none:1")
	if ev1 == nil || ev1.Severity != "warning" {
		t.Errorf("card1 should be warning: %+v", ev1)
	}
	// Card 7 → warning (non-confirmed quadrant).
	ev7 := findEmit(events, "none:7")
	if ev7 == nil || ev7.Severity != "warning" {
		t.Errorf("card7 should be warning: %+v", ev7)
	}
}

func TestEmitToFaultSubEmptyURLNoOp(t *testing.T) {
	// No URL configured → no panic, no requests.
	EmitToFaultSub(&DetectionResult{Results: []CardDetectionSummary{{CardID: 0, Quadrant: QuadConfirmedAnomaly}}}, EmitConfig{URL: ""})
	EmitToFaultSub(nil, EmitConfig{URL: "http://localhost:9101"})
}

func TestEmitToFaultSubServerDown(t *testing.T) {
	// A failing faultsub must not abort; events are logged to stderr.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "down", http.StatusInternalServerError)
	}))
	defer srv.Close()
	result := &DetectionResult{
		Results: []CardDetectionSummary{{CardID: 0, Quadrant: QuadConfirmedAnomaly, AnomalyCategory: CatCompute, CompositeScore: 9}},
	}
	EmitToFaultSub(result, EmitConfig{URL: srv.URL, Timeout: 0})
	// No panic, function returns. (Log lines go to stderr.)
}

func findEmit(events []FaultEvent, npu string) *FaultEvent {
	for i := range events {
		if events[i].NPUID == npu {
			return &events[i]
		}
	}
	return nil
}
