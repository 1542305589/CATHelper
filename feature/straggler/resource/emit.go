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
	Severity  string            `json:"severity"`           // critical | warning
	Detail    map[string]string `json:"detail,omitempty"`   // metric → space_score
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
// Severity is derived from the card's worst space degradation (>= 5 → critical,
// else warning). Cards with no anomaly are skipped. Errors are logged to
// stderr; detection proceeds regardless (a faultsub outage must not block the
// report).
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

	for _, ev := range anomalousCardEvents(result) {
		if err := postEvent(client, endpoint, ev); err != nil {
			fmt.Fprintf(os.Stderr, "[SLOWNODE ALGO] [WARN] faultsub emit %s failed: %v\n", ev.NPUID, err)
		}
	}
}

// anomalousCardEvents flattens the metric-first result back to one event per
// anomalous card. Detail maps each anomalous metric name to its space_score.
func anomalousCardEvents(result *DetectionResult) []FaultEvent {
	type cardAgg struct {
		node   string
		card   int
		worst  float64
		detail map[string]string
	}
	byKey := make(map[string]*cardAgg)
	var order []string

	add := func(node string, card int) *cardAgg {
		key := node + ":" + strconv.Itoa(card)
		a, ok := byKey[key]
		if !ok {
			a = &cardAgg{node: node, card: card, detail: make(map[string]string)}
			byKey[key] = a
			order = append(order, key)
		}
		return a
	}

	for _, m := range result.Metrics {
		for _, c := range m.Cards {
			if !c.SpaceAbnormal {
				continue
			}
			a := add(c.Node, c.CardID)
			if c.SpaceScore > a.worst {
				a.worst = c.SpaceScore
			}
			a.detail[string(m.Metric)] = strconv.FormatFloat(c.SpaceScore, 'f', -1, 64)
		}
	}

	events := make([]FaultEvent, 0, len(order))
	for _, key := range order {
		a := byKey[key]
		sev := "warning"
		if a.worst >= 5 {
			sev = "critical"
		}
		events = append(events, FaultEvent{
			Type:      "straggler_detected",
			Component: "npu",
			NPUID:     a.node + ":" + strconv.Itoa(a.card),
			Severity:  sev,
			Detail:    a.detail,
			Timestamp: time.Now(),
		})
	}
	return events
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
