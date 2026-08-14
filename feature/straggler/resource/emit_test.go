package resource

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
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
		Metrics: []MetricAnomaly{
			{Metric: MetricTemp, SpaceMethod: MethodCluster, Cards: []AnomalousCard{
				{Node: noneNode, CardID: 3, SpaceScore: 8.7, SpaceAbnormal: true},
				{Node: noneNode, CardID: 1, SpaceScore: 5.1, SpaceAbnormal: true},
			}},
			{Metric: MetricAICoreUtil, SpaceMethod: MethodCluster, Cards: []AnomalousCard{
				{Node: noneNode, CardID: 7, SpaceScore: 3.2, SpaceAbnormal: true},
			}},
			// A normal card (debug-style entry, space_abnormal=false) is skipped.
			{Metric: MetricTXBandwidth, SpaceMethod: MethodCluster, Cards: []AnomalousCard{
				{Node: noneNode, CardID: 2, SpaceScore: 1.0, SpaceAbnormal: false},
			}},
		},
	}
	EmitToFaultSub(result, EmitConfig{URL: srv.URL, Timeout: 0})

	mu.Lock()
	defer mu.Unlock()
	if len(events) != 3 {
		t.Fatalf("expected 3 events (3 anomalous, normal skipped), got %d", len(events))
	}
	// Card 3 → critical (worst space score 8.7 >= 5).
	ev3 := findEmit(events, noneNode+":3")
	if ev3 == nil {
		t.Fatal("missing card 3 event")
	}
	if ev3.Type != "straggler_detected" || ev3.Severity != "critical" {
		t.Errorf("card3 event wrong: %+v", ev3)
	}
	if ev3.Detail["temp"] != "8.7" {
		t.Errorf("card3 detail wrong: %+v", ev3.Detail)
	}
	// Card 1 → critical (worst 5.1 >= 5).
	ev1 := findEmit(events, noneNode+":1")
	if ev1 == nil || ev1.Severity != "critical" {
		t.Errorf("card1 should be critical (5.1): %+v", ev1)
	}
	// Card 7 → warning (worst 3.2 < 5).
	ev7 := findEmit(events, noneNode+":7")
	if ev7 == nil || ev7.Severity != "warning" {
		t.Errorf("card7 should be warning (3.2): %+v", ev7)
	}
	// Normal card 2 must not be emitted.
	if findEmit(events, noneNode+":2") != nil {
		t.Errorf("normal card 2 should not be emitted")
	}
}

func TestEmitToFaultSubEmptyURLNoOp(t *testing.T) {
	// No URL configured → no panic, no requests.
	EmitToFaultSub(&DetectionResult{Metrics: []MetricAnomaly{
		{Metric: MetricTemp, Cards: []AnomalousCard{{Node: noneNode, CardID: 0, SpaceScore: 2, SpaceAbnormal: true}}},
	}}, EmitConfig{URL: ""})
	EmitToFaultSub(nil, EmitConfig{URL: "http://localhost:9101"})
}

func TestEmitToFaultSubServerDown(t *testing.T) {
	// A failing faultsub must not abort; events are logged to stderr.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "down", http.StatusInternalServerError)
	}))
	defer srv.Close()
	result := &DetectionResult{
		Metrics: []MetricAnomaly{
			{Metric: MetricTemp, Cards: []AnomalousCard{{Node: noneNode, CardID: 0, SpaceScore: 9, SpaceAbnormal: true}}},
		},
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
