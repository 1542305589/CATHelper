// Package config provides global configuration and result types for the
// straggler (slow-node) detection system.
package config

import (
	"sort"
	"strconv"
	"strings"
)

// Global configuration variables 鈥?the ACTIVE thresholds read by the detector
// and report. They are set per scope via Apply: once at startup in one-shot
// mode, per cycle in daemon mode, per business in center mode.
var (
	FilePath      string  // Data directory containing ascend_pytorch_profiler_*.db files.
	CalThreshold  float64 // Slow-compute ratio threshold (default 1.3).
	CPUThreshold  float64 // IO/CPU ratio threshold (default 2.5), used by cpu and memcpy_async.
	CommThreshold float64 // Slow-comm bandwidth kmeans ratio threshold (default 1.3).
	SlowCommFlat  bool    // When true, compute bandwidth from the flat pool of ALL ranks' op durations (no cross-rank alignment).
	PPSessionNs   int64   // PP session length (max merged profiling window) for pp_comm detection.
)

// Fixed comm-bandwidth filters, mirroring the Python skill's
// SLOW_COMM_MIN_COUNT / SLOW_COMM_COUNT_FLOOR. They are deliberately not
// tunable (no CLI flag / API field), matching the reference.
const (
	SlowCommMinCount   = 1000
	SlowCommCountFloor = 10240
)

// Thresholds is the full set of independent detection thresholds. It is
// persisted per business (center), held per daemon (daemon), or applied once
// (one-shot). Zero values fall back to DefaultThresholds.
type Thresholds struct {
	Cal           float64 `json:"cal"`
	CPU           float64 `json:"cpu"`
	CommThreshold float64 `json:"comm_threshold"`
}

// DefaultThresholds returns the defaults used for any unset threshold.
func DefaultThresholds() Thresholds {
	return Thresholds{Cal: 1.3, CPU: 2.5, CommThreshold: 1.3}
}

// Normalized fills any non-positive threshold with its default.
func (t Thresholds) Normalized() Thresholds {
	d := DefaultThresholds()
	if t.Cal <= 0 {
		t.Cal = d.Cal
	}
	if t.CPU <= 0 {
		t.CPU = d.CPU
	}
	if t.CommThreshold <= 0 {
		t.CommThreshold = d.CommThreshold
	}
	return t
}

// Apply sets the active global thresholds (normalizing unset values first).
func Apply(t Thresholds) {
	t = t.Normalized()
	CalThreshold = t.Cal
	CPUThreshold = t.CPU
	CommThreshold = t.CommThreshold
}

// Current returns the active global thresholds.
func Current() Thresholds {
	return Thresholds{
		Cal:           CalThreshold,
		CPU:           CPUThreshold,
		CommThreshold: CommThreshold,
	}
}

// DegradationData is the aggregated result of all detection categories.
//
// Outer keys: "cal", "kernel_aivec", "memcpy_async", "comm", "pp_comm", "cpu".
// Inner keys:
//   - single-card:  strconv.Itoa(rank), e.g. "0", "15"
//   - group:        comma-separated sorted ranks,   e.g. "0,2,4"
type DegradationData map[string]map[string]float64

// NewDegradationData allocates an empty result map.
func NewDegradationData() DegradationData {
	return make(DegradationData)
}

// ensureCategory lazily creates the inner map for a category.
func (d DegradationData) ensureCategory(category string) {
	if d[category] == nil {
		d[category] = make(map[string]float64)
	}
}

// AddSingle records a single-card detection result.
func (d DegradationData) AddSingle(category string, rank int, degradation float64) {
	d.ensureCategory(category)
	d[category][singleKey(rank)] = degradation
}

// AddGroup records a group-level detection result.
// If the same group has already been recorded the larger degradation value wins.
func (d DegradationData) AddGroup(category string, ranks []int, degradation float64) {
	d.ensureCategory(category)
	key := groupKey(ranks)
	if prev, ok := d[category][key]; !ok || degradation > prev {
		d[category][key] = degradation
	}
}

// AddKey records a result under an arbitrary display key (e.g. a node name).
func (d DegradationData) AddKey(category, key string, degradation float64) {
	d.ensureCategory(category)
	d[category][key] = degradation
}

// ---------------------------------------------------------------------------
// Private helpers
// ---------------------------------------------------------------------------

func singleKey(rank int) string {
	return strconv.Itoa(rank)
}

func groupKey(ranks []int) string {
	sorted := make([]int, len(ranks))
	copy(sorted, ranks)
	sort.Ints(sorted)
	parts := make([]string, len(sorted))
	for i, r := range sorted {
		parts[i] = strconv.Itoa(r)
	}
	return strings.Join(parts, ",")
}
