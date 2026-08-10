package resource

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"time"
)

// FaultEvent is the event straggler POSTs back to CATMonitor's faultsub REST
// ingest endpoint (POST /faultsub/events). It mirrors faultsub.FaultEvent's
// JSON contract; kept as a local struct so straggler (an independent module)
// does not import CATMonitor packages.
type FaultEvent struct {
	EventID   string            `json:"event_id"`           // filled server-side if empty
	Type      string            `json:"type"`               // "straggler_detected"
	Component string            `json:"component"`          // "npu"
	NPUID     string            `json:"npu_id"`
	Severity  string            `json:"severity"`           // critical | warning | info
	Detail    map[string]string `json:"detail,omitempty"`
	Timestamp time.Time         `json:"timestamp"`          // filled server-side if zero
	Recovered bool              `json:"recovered"`
}

// EmitConfig controls the faultsub回注 behaviour.
type EmitConfig struct {
	URL     string        // faultsub REST base URL, e.g. http://localhost:9101
	Timeout time.Duration // per-request timeout (default 10s)
}

// EmitToFaultSub POSTs one straggler_detected event per anomalous card in the
// detection result to faultsub's ingest endpoint (POST {URL}/faultsub/events).
// Confirmed anomalies → critical; early degradation → warning; individual
// variance → info (carried but low priority). Cards that are QuadNormal are
// skipped. Errors are logged to stderr; detection proceeds regardless (a
// faultsub outage must not block the report).
func EmitToFaultSub(result *DetectionResult, cfg EmitConfig) {
	if cfg.URL == "" || result == nil {
		return
	}
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	client := &http.Client{Timeout: timeout}
	endpoint := cfg.URL + "/faultsub/events"

	rootCauseByCard := map[int]RootCauseResult{}
	for _, rc := range result.RootCauses {
		rootCauseByCard[rc.CardID] = rc
	}

	for _, s := range result.Results {
		if s.Quadrant == QuadNormal {
			continue
		}
		ev := buildEvent(s, rootCauseByCard[s.CardID])
	if err := postEvent(client, endpoint, ev); err != nil {
		fmt.Fprintf(os.Stderr, "[SLOWNODE ALGO] [WARN] faultsub emit card %d failed: %v\n", s.CardID, err)
	}
}
}

// buildEvent assembles a straggler_detected FaultEvent for one card.
func buildEvent(s CardDetectionSummary, rc RootCauseResult) FaultEvent {
	sev := "warning"
	switch s.Quadrant {
	case QuadConfirmedAnomaly:
		sev = "critical"
	case QuadIndividualVariance:
		sev = "info"
	}
	detail := map[string]string{
		"quadrant":          s.Quadrant.String(),
		"anomaly_category":  string(s.AnomalyCategory),
		"composite_score":   strconv.FormatFloat(s.CompositeScore, 'f', -1, 64),
	}
	if rc.Category != "" {
		detail["root_cause"] = string(rc.Category)
		if rc.Suggestion != "" {
			detail["suggestion"] = rc.Suggestion
		}
		detail["confidence"] = string(rc.Confidence)
	}
	// NPUID includes the node so cards from different nodes with the same
	// per-node card ID do not collide in faultsub.
	node := s.Node
	if node == "" {
		node = noneNode
	}
	return FaultEvent{
		Type:      "straggler_detected",
		Component: "npu",
		NPUID:     node + ":" + strconv.Itoa(s.CardID),
		Severity:  sev,
		Detail:    detail,
		Timestamp: time.Now(),
	}
}

// postEvent POSTs one event JSON to the endpoint; non-2xx is an error.
func postEvent(client *http.Client, endpoint string, ev FaultEvent) error {
	body, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	req, err := http.NewRequest(http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-CatMonitor-Event", ev.Type)
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("faultsub ingest HTTP %d", resp.StatusCode)
	}
	return nil
}
