// Package center implements the center-node mode: a manager that controls
// multiple businesses (real training tasks). Each business maps to one or more
// daemons (each a slowNodeDetection --daemon); the center matches those daemons,
// triggers them on its own timer, receives their reported op_metric JSON, merges
// and re-runs detection across the whole business, and serves a web console.
package center

import (
	"fmt"
	"time"

	"github.com/Computing-Availability-Tools/CATHelper/feature/straggler/config"
	"github.com/Computing-Availability-Tools/CATHelper/feature/straggler/profiling/detector"
)

// Daemon is one daemon within a business. Key is persisted so a center restart
// can re-authenticate to an already-matched daemon (whose own key survives in
// memory); collectWait / health / reported op_metric remain runtime-only and
// are rebuilt on re-match.
type Daemon struct {
	IP   string `json:"ip"`
	Port int    `json:"port"`
	Key  string `json:"key,omitempty"` // per-daemon match secret, persisted across center restarts

	collectWait int64  // daemon's --collect-wait (seconds), from /daemon/match
	healthy     bool   // last healthz probe result
	failCount   int    // consecutive healthz failures
	matchState  string // "", "healthy", "unmatched", "other"

	lastOpMetric detector.OpMetric // most recent reported op_metric (in-memory)
	lastReportAt time.Time         // when the last report arrived
}

// Addr returns "ip:port".
func (d *Daemon) Addr() string { return fmt.Sprintf("%s:%d", d.IP, d.Port) }

// BaseURL returns "http://ip:port".
func (d *Daemon) BaseURL() string { return fmt.Sprintf("http://%s:%d", d.IP, d.Port) }

// Business is one managed training task covering 1..N daemons (globally unique
// ranks across them). Paused / cycle counters are persisted; nextTrigger is
// runtime-only.
type Business struct {
	Name                   string            `json:"name"`
	IntervalSec            int64             `json:"interval_sec"`
	Daemons                []*Daemon         `json:"daemons"`
	Paused                 bool              `json:"paused,omitempty"`
	CyclesTotal            int               `json:"cycles_total"`
	CyclesFailed           int               `json:"cycles_failed"`
	VLLMMetrics            string            `json:"vllm_metrics,omitempty"`              // vllm /metrics endpoint URL (one per business)
	Thresholds             config.Thresholds `json:"thresholds"`                          // merged-detection thresholds (zero = inherit center default)
	KPIDetectIntervalSec   int64             `json:"kpi_interval_sec,omitempty"`          // KPI detection period (independent of profiler)
	KPINSigma              KPINSigma         `json:"kpi_nsigma,omitempty"`                // per-metric KPI n-sigma thresholds
	KPIReferenceAdmitRatio float64           `json:"kpi_reference_admit_ratio,omitempty"` // anomaly points admitted into reference (0..1)
	ReferenceSince         map[string]int64  `json:"reference_since,omitempty"`           // node -> reference window start ts

	nextTrigger              time.Time    // next scheduled profiler trigger (in-memory)
	triggering               bool         // a profiler trigger round is currently in flight (in-memory, single-flight)
	nextKPIDetect            time.Time    // next scheduled KPI detection (in-memory)
	kpiDetecting             bool         // a KPI detection round is currently in flight (in-memory, single-flight)
	kpiAnomalyStreak         int          // consecutive KPI detection runs with any anomaly (in-memory)
	kpiProfilerCooldownUntil time.Time    // KPI-triggered profiler cooldown (in-memory)
	lastTriggerReason        string       // "timer" | "kpi" | "manual" (in-memory)
	progress                 *progressLog // live per-round stage log for the console (in-memory)
}

// Config holds the center's runtime configuration (--center CLI flags).
// KPINSigma holds the per-metric, per-dimension n-sigma thresholds for KPI
// detection. Zero values are replaced by DefaultKPINSigma().
type KPINSigma struct {
	PowerTime  float64 `json:"power_time"`
	PowerSpace float64 `json:"power_space"`
	UtilTime   float64 `json:"util_time"`
	UtilSpace  float64 `json:"util_space"`
	FreqTime   float64 `json:"freq_time"`
	FreqSpace  float64 `json:"freq_space"`
}

// DefaultKPIReferenceAdmitRatio is the default probability that an anomalous
// point is still admitted into the reference window.
const DefaultKPIReferenceAdmitRatio = 0.1

// NormalizedKPIReferenceAdmitRatio fills invalid values with the default.
func (b *Business) NormalizedKPIReferenceAdmitRatio() float64 {
	if b.KPIReferenceAdmitRatio <= 0 || b.KPIReferenceAdmitRatio > 1 {
		return DefaultKPIReferenceAdmitRatio
	}
	return b.KPIReferenceAdmitRatio
}

// DefaultKPINSigma returns the default per-metric KPI n-sigma thresholds.
func DefaultKPINSigma() KPINSigma {
	return KPINSigma{
		PowerTime: 4, PowerSpace: 4,
		UtilTime: 4, UtilSpace: 4,
		FreqTime: 3, FreqSpace: 3,
	}
}

// Normalized fills non-positive fields with defaults.
func (n KPINSigma) Normalized() KPINSigma {
	d := DefaultKPINSigma()
	if n.PowerTime <= 0 {
		n.PowerTime = d.PowerTime
	}
	if n.PowerSpace <= 0 {
		n.PowerSpace = d.PowerSpace
	}
	if n.UtilTime <= 0 {
		n.UtilTime = d.UtilTime
	}
	if n.UtilSpace <= 0 {
		n.UtilSpace = d.UtilSpace
	}
	if n.FreqTime <= 0 {
		n.FreqTime = d.FreqTime
	}
	if n.FreqSpace <= 0 {
		n.FreqSpace = d.FreqSpace
	}
	return n
}

// TimeN returns the time-dimension n for a metric.
func (n KPINSigma) TimeN(metric string) float64 {
	n = n.Normalized()
	switch metric {
	case "power":
		return n.PowerTime
	case "aicore_util":
		return n.UtilTime
	case "aicore_freq":
		return n.FreqTime
	}
	return 0
}

// SpaceN returns the space-dimension n for a metric.
func (n KPINSigma) SpaceN(metric string) float64 {
	n = n.Normalized()
	switch metric {
	case "power":
		return n.PowerSpace
	case "aicore_util":
		return n.UtilSpace
	case "aicore_freq":
		return n.FreqSpace
	}
	return 0
}

type Config struct {
	Port              int               // HTTP listen port
	DataDir           string            // persistence root (businesses.json + per-business results)
	Interval          time.Duration     // default profiler trigger period for new businesses
	KPIDetectInterval time.Duration     // default KPI detection period for new businesses
	Thresholds        config.Thresholds // default merged-detection thresholds for new businesses
}

// DefaultConfig returns sensible defaults.
func DefaultConfig() Config {
	return Config{
		Port:              8080,
		DataDir:           "center_data",
		Interval:          10 * time.Minute,
		KPIDetectInterval: 10 * time.Second,
		Thresholds:        config.DefaultThresholds(),
	}
}

// persistedState is what survives a restart (including match keys).
type persistedState struct {
	Businesses []*Business `json:"businesses"`
}
