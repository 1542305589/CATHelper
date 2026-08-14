// Package resource implements NPU resource KPI anomaly detection using
// space-dimension peer comparison.
//
// Detection pipeline:
//   CSV/JSONL parse → aggregation-window (10s) trimmed-mean aggregation →
//   space detection (peer comparison, last aggregated point) → compute-first
//   fusion → root-cause bounding → JSON + text report
package resource

import (
	"math"
)

// =============================================================================
// Raw Data Types
// =============================================================================

// CSVRow is one row of raw CSV data. Each metric is a JSON dict keyed by card ID.
type CSVRow struct {
	Timestamp      int64
	Power          map[int]float64 // cardID → watts
	Temp           map[int]float64 // cardID → celsius
	AICoreFreq     map[int]float64 // cardID → MHz
	AICoreUtil     map[int]float64 // cardID → %
	HBMBandwidthUtil map[int]float64 // cardID → % (bandwidth utilization)
	HBMUtil          map[int]float64 // cardID → % (memory utilization)
	TXBandwidth    map[int]float64 // cardID → ?
	RXPfcPkt       map[int]float64 // cardID → packets (cumulative counter)
	RocETxErrPkt   map[int]float64 // cardID → packets (cumulative counter)
	RocEOutOfOrder map[int]float64 // cardID → packets (cumulative counter)
	RocENewPktRty  map[int]float64 // cardID → packets (cumulative counter)
	NICRxAllPkg    map[int]float64 // cardID → packets
	CPUAvg         map[string]string // cpuName → utilization %
}

// TimeSeriesData holds the complete parsed time series split into windows.
type TimeSeriesData struct {
	Rows    []CSVRow // aggregated rows (1 per aggregation window)
	CardIDs []int    // all global card IDs found in the data
	RawRows []CSVRow // raw rows before aggregation (for counter calculations)
	// NodeOf maps each global card ID to its node name; LocalID maps it back to
	// the per-node card ID (0-based within the node). Flat (single-node) input
	// assigns every card node "none" and LocalID == global ID.
	NodeOf map[int]string
	LocalID map[int]int
}

// =============================================================================
// Metric Enumeration
// =============================================================================

// noneNode is the node name assigned to flat (single-node) inputs, where the
// metric JSON is {cardID: value} without a node layer.
const noneNode = "none"

// MetricName enumerates all NPU resource metrics.
type MetricName string

const (
	MetricTemp           MetricName = "temp"
	MetricPower          MetricName = "power"
	MetricAICoreFreq     MetricName = "aicore_freq"
	MetricAICoreUtil     MetricName = "aicore_util"
	MetricHBMBandwidthUtil MetricName = "hbm_bandwidth_util"
	MetricHBMUtil         MetricName = "hbm_util"
	MetricTXBandwidth    MetricName = "tx_bandwidth"
	MetricRXPfcPkt       MetricName = "rx_pfc_pkt"
	MetricRocETxErrPkt   MetricName = "roce_tx_err_pkt"
	MetricRocEOutOfOrder MetricName = "roce_out_of_order"
	MetricRocENewPktRty  MetricName = "roce_new_pkt_rty"
)

// AllMetrics is the ordered list of all metrics for iteration.
var AllMetrics = []MetricName{
	MetricTemp,
	MetricPower,
	MetricAICoreFreq,
	MetricAICoreUtil,
	MetricHBMBandwidthUtil,
	MetricHBMUtil,
	MetricTXBandwidth,
	MetricRXPfcPkt,
	MetricRocETxErrPkt,
	MetricRocEOutOfOrder,
	MetricRocENewPktRty,
}

// ComputeMetrics lists metrics classified as compute-related.
var ComputeMetrics = map[MetricName]bool{
	MetricTemp:       true,
	MetricPower:      true,
	MetricAICoreFreq: true,
	MetricAICoreUtil: true,
	MetricHBMBandwidthUtil:    true,
	MetricHBMUtil:             true,
}

// CommunicationMetrics lists metrics classified as communication-related.
var CommunicationMetrics = map[MetricName]bool{
	MetricTXBandwidth:    true,
	MetricRXPfcPkt:       true,
	MetricRocETxErrPkt:   true,
	MetricRocEOutOfOrder: true,
	MetricRocENewPktRty:  true,
}

// CounterMetrics lists metrics that are cumulative counters (use accumulation,
// not trimmed mean, during aggregation).
var CounterMetrics = map[MetricName]bool{
	MetricRXPfcPkt:       true,
	MetricRocETxErrPkt:   true,
	MetricRocEOutOfOrder: true,
	MetricRocENewPktRty:  true,
}

// IsComputeMetric reports whether m is a compute-class metric.
func IsComputeMetric(m MetricName) bool { return ComputeMetrics[m] }

// IsCommunicationMetric reports whether m is a communication-class metric.
func IsCommunicationMetric(m MetricName) bool { return CommunicationMetrics[m] }

// IsCounterMetric reports whether m is a cumulative counter.
func IsCounterMetric(m MetricName) bool { return CounterMetrics[m] }

// =============================================================================
// Metric Direction & Detection Method
// =============================================================================

// AnomalyDirection indicates whether abnormal means "too high" or "too low".
type AnomalyDirection int

const (
	DirHigh AnomalyDirection = iota // abnormally high
	DirLow                          // abnormally low
)

// DetectionMethod selects the statistical method for space-dimension detection.
type DetectionMethod string

const (
	MethodZScore   DetectionMethod = "zscore"
	MethodIQR      DetectionMethod = "iqr"
	MethodDirect   DetectionMethod = "direct"   // direct comparison (no metric currently uses it)
	MethodAbsolute DetectionMethod = "absolute" // > threshold → anomaly
	MethodCluster  DetectionMethod = "cluster"  // majority-mode clustering
)

// MetricMeta describes the detection parameters for a single metric.
type MetricMeta struct {
	Name         MetricName
	Category     AnomalyCategory
	Direction    AnomalyDirection
	SpaceMethod  DetectionMethod
	AbsThreshold float64 // for MethodAbsolute
}

// MetricMetaRegistry maps each metric to its meta-information.
var MetricMetaRegistry = map[MetricName]MetricMeta{
	MetricTemp:           {Name: MetricTemp, Category: CatCompute, Direction: DirHigh, SpaceMethod: MethodCluster},
	MetricPower:          {Name: MetricPower, Category: CatCompute, Direction: DirHigh, SpaceMethod: MethodCluster},
	MetricAICoreFreq:     {Name: MetricAICoreFreq, Category: CatCompute, Direction: DirLow, SpaceMethod: MethodCluster},
	MetricAICoreUtil:     {Name: MetricAICoreUtil, Category: CatCompute, Direction: DirLow, SpaceMethod: MethodCluster},
	MetricHBMBandwidthUtil:        {Name: MetricHBMBandwidthUtil, Category: CatCompute, Direction: DirLow, SpaceMethod: MethodCluster},
	MetricHBMUtil:         {Name: MetricHBMUtil, Category: CatCompute, Direction: DirLow, SpaceMethod: MethodCluster},
	MetricTXBandwidth:    {Name: MetricTXBandwidth, Category: CatCommunication, Direction: DirLow, SpaceMethod: MethodCluster},
	MetricRXPfcPkt:       {Name: MetricRXPfcPkt, Category: CatCommunication, Direction: DirHigh, SpaceMethod: MethodAbsolute, AbsThreshold: 0},
	MetricRocETxErrPkt:   {Name: MetricRocETxErrPkt, Category: CatCommunication, Direction: DirHigh, SpaceMethod: MethodAbsolute, AbsThreshold: 0},
	MetricRocEOutOfOrder: {Name: MetricRocEOutOfOrder, Category: CatCommunication, Direction: DirHigh, SpaceMethod: MethodAbsolute, AbsThreshold: 0},
	MetricRocENewPktRty:  {Name: MetricRocENewPktRty, Category: CatCommunication, Direction: DirHigh, SpaceMethod: MethodAbsolute, AbsThreshold: 0},
}

// =============================================================================
// Detection Results
// =============================================================================

// AnomalyCategory classifies the anomaly as compute, communication, or none.
type AnomalyCategory string

const (
	CatNone          AnomalyCategory = "none"
	CatCompute       AnomalyCategory = "compute"
	CatCommunication AnomalyCategory = "communication"
)

// Quadrant is the card-level anomaly state. With the time dimension removed it
// is decided purely by the space dimension: space-abnormal → confirmed_anomaly,
// else normal. QuadEarlyDegradation / QuadIndividualVariance are retained for
// output compatibility but never produced.
type Quadrant int

const (
	QuadNormal            Quadrant = iota // no space anomaly
	QuadEarlyDegradation                 // retained (not produced)
	QuadIndividualVariance               // retained (not produced)
	QuadConfirmedAnomaly                 // space-abnormal
)

func (q Quadrant) String() string {
	switch q {
	case QuadNormal:
		return "normal"
	case QuadEarlyDegradation:
		return "early_degradation"
	case QuadIndividualVariance:
		return "individual_variance"
	case QuadConfirmedAnomaly:
		return "confirmed_anomaly"
	default:
		return "unknown"
	}
}

// MetricAnomalyDetail records the space-dimension anomaly score for one metric
// on one card.
type MetricAnomalyDetail struct {
	Metric        MetricName      `json:"metric"`
	SpaceScore    float64         `json:"space_score"`
	SpaceAbnormal bool            `json:"space_abnormal"`
	Quadrant      Quadrant        `json:"quadrant"`
	SpaceMethod   DetectionMethod `json:"space_method"`
}

// CardDetectionSummary is the per-card detection result.
// CardID is the per-node card ID (0-based within the node) at output time;
// Node disambiguates cards across nodes. Set in applyNodeIdentity (report.go).
type CardDetectionSummary struct {
	CardID                 int                   `json:"card_id"`
	Node                   string                `json:"node"`
	AnomalyCategory        AnomalyCategory       `json:"anomaly_category"`
	Quadrant               Quadrant              `json:"quadrant"`
	AnomalyDetails         []MetricAnomalyDetail `json:"anomaly_details,omitempty"`
	SecondaryCommAnomalies []MetricAnomalyDetail `json:"secondary_comm_anomalies,omitempty"`
	CompositeScore         float64               `json:"composite_score"`
	Severity               Severity              `json:"severity"`
}

// =============================================================================
// Severity Enums
// =============================================================================

// Severity indicates how urgent the finding is.
type Severity string

const (
	SevCritical Severity = "critical"
	SevWarning  Severity = "warning"
	SevInfo     Severity = "info"
)

// =============================================================================
// Detection Config
// =============================================================================

// DetectionConfig holds all tunable parameters for KPI anomaly detection.
type DetectionConfig struct {
	// Preprocessing
	AggregationWindowSec int     // aggregation window in seconds, default 10
	TrimRatio            float64 // trimming ratio, default 0.25 (25% each side)
	MinSamplesForTrim    int     // minimum samples to apply trimming, default 4

	// Space dimension
	SpaceZThreshold     float64 // default 2.5
	SpaceIQRMult        float64 // default 1.5
	SpaceRatioThreshold float64 // kmeans cluster ratio threshold (cluster mean / baseline mean), default 2.0

	// Debug
	EnableDebug bool // --debug-output: include all cards × all metrics in the result

	// Profiling integration
	FallbackToProfiling bool
	AlwaysRunProfiling  bool
}

// DefaultDetectionConfig returns a DetectionConfig with sensible defaults.
func DefaultDetectionConfig() DetectionConfig {
	return DetectionConfig{
		AggregationWindowSec: 10,
		TrimRatio:            0.25,
		MinSamplesForTrim:    4,

		SpaceZThreshold:     2.5,
		SpaceIQRMult:        1.5,
		SpaceRatioThreshold: 2.0,

		FallbackToProfiling: true,
		AlwaysRunProfiling:  false,
	}
}

// =============================================================================
// Detection Result (top-level)
// =============================================================================

// DetectionResult is the complete KPI detection output.
type DetectionResult struct {
	Summary DetectionSummary       `json:"summary"`
	Results []CardDetectionSummary `json:"results"`
}

// DetectionSummary is the overview section of the output.
type DetectionSummary struct {
	TotalCards         int    `json:"total_cards"`
	TotalNodes         int    `json:"total_nodes"`
	ConfirmedAnomalies int    `json:"confirmed_anomalies"`
	EarlyDegradation   int    `json:"early_degradation"`
	IndividualVariance int    `json:"individual_variance"`
	Normal             int    `json:"normal"`
	KPICSV             string `json:"kpi_csv"`
	TotalTimePoints    int    `json:"total_time_points"`
}

// SpaceDetectionResult holds per-time-point space anomaly scores.
type SpaceDetectionResult struct {
	Scores map[int]map[MetricName][]float64
}

// =============================================================================
// Helpers
// =============================================================================

// MeanStd calculates the mean and standard deviation of a float64 slice.
func MeanStd(values []float64) (mean, std float64) {
	if len(values) == 0 {
		return 0, 0
	}
	n := float64(len(values))
	var sum float64
	for _, v := range values {
		sum += v
	}
	mean = sum / n
	if len(values) < 2 {
		return mean, 0
	}
	var sqSum float64
	for _, v := range values {
		d := v - mean
		sqSum += d * d
	}
	std = math.Sqrt(sqSum / (n - 1))
	return
}

// MinMax returns the min and max of a float64 slice.
func MinMax(values []float64) (min, max float64) {
	if len(values) == 0 {
		return 0, 0
	}
	min, max = values[0], values[0]
	for _, v := range values[1:] {
		if v < min {
			min = v
		}
		if v > max {
			max = v
		}
	}
	return
}

// Percentile calculates the p-th percentile (0..1) of sorted values.
func Percentile(sorted []float64, p float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	if p <= 0 {
		return sorted[0]
	}
	if p >= 1 {
		return sorted[len(sorted)-1]
	}
	k := p * float64(len(sorted)-1)
	f := math.Floor(k)
	c := math.Ceil(k)
	if f == c {
		return sorted[int(k)]
	}
	return sorted[int(f)]*(c-k) + sorted[int(c)]*(k-f)
}

// HasConfirmedAnomaly reports whether any card has a confirmed anomaly.
func HasConfirmedAnomaly(summaries []CardDetectionSummary) bool {
	for _, s := range summaries {
		if s.Quadrant == QuadConfirmedAnomaly {
			return true
		}
	}
	return false
}
