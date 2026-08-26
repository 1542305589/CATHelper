// Package resource implements NPU resource KPI anomaly detection using
// space-dimension peer comparison.
//
// Detection pipeline:
//   CSV/JSONL parse → aggregation-window (10s) trimmed-mean aggregation →
//   space detection (peer comparison, last aggregated point) → compute-first
//   fusion → root-cause bounding → JSON + text report
package resource

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
// Detection Method
// =============================================================================

// DetectionMethod selects the statistical method for space-dimension detection.
// The cluster method's anomaly direction is NOT pre-decided: both directions
// are run and the side flagging fewer cards is reported (see space_detector.go).
type DetectionMethod string

const (
	MethodAbsolute DetectionMethod = "absolute" // > threshold → anomaly
	MethodCluster  DetectionMethod = "cluster"  // majority-mode clustering
)

// MetricMeta describes the detection parameters for a single metric.
type MetricMeta struct {
	Name         MetricName
	Category     AnomalyCategory
	Method       DetectionMethod
	AbsThreshold float64 // for MethodAbsolute
}

// MetricMetaRegistry maps each metric to its meta-information.
var MetricMetaRegistry = map[MetricName]MetricMeta{
	MetricTemp:           {Name: MetricTemp, Category: CatCompute, Method: MethodCluster},
	MetricPower:          {Name: MetricPower, Category: CatCompute, Method: MethodCluster},
	MetricAICoreFreq:     {Name: MetricAICoreFreq, Category: CatCompute, Method: MethodCluster},
	MetricAICoreUtil:     {Name: MetricAICoreUtil, Category: CatCompute, Method: MethodCluster},
	MetricHBMBandwidthUtil:        {Name: MetricHBMBandwidthUtil, Category: CatCompute, Method: MethodCluster},
	MetricHBMUtil:         {Name: MetricHBMUtil, Category: CatCompute, Method: MethodCluster},
	MetricTXBandwidth:    {Name: MetricTXBandwidth, Category: CatCommunication, Method: MethodCluster},
	MetricRXPfcPkt:       {Name: MetricRXPfcPkt, Category: CatCommunication, Method: MethodAbsolute, AbsThreshold: 0},
	MetricRocETxErrPkt:   {Name: MetricRocETxErrPkt, Category: CatCommunication, Method: MethodAbsolute, AbsThreshold: 0},
	MetricRocEOutOfOrder: {Name: MetricRocEOutOfOrder, Category: CatCommunication, Method: MethodAbsolute, AbsThreshold: 0},
	MetricRocENewPktRty:  {Name: MetricRocENewPktRty, Category: CatCommunication, Method: MethodAbsolute, AbsThreshold: 0},
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

// MetricAnomalyDetail records the space-dimension anomaly score for one metric
// on one card (internal detection detail; the output groups anomalies by
// metric, see MetricAnomaly).
type MetricAnomalyDetail struct {
	Metric        MetricName      `json:"metric"`
	Score    float64         `json:"score"`
	Abnormal bool            `json:"abnormal,omitempty"`
	Method   DetectionMethod `json:"method"`
}

// AnomalousCard is one card anomalous for a metric, with its space degradation
// degree (score).
type AnomalousCard struct {
	Node          string  `json:"node"`
	CardID        int     `json:"card_id"` // node-local card ID (0-based)
	Score    float64 `json:"score"`
	Abnormal bool    `json:"abnormal,omitempty"`
}

// MetricAnomaly groups the anomalous cards of one metric.
type MetricAnomaly struct {
	Metric      MetricName      `json:"metric"`
	Method DetectionMethod `json:"method"`
	Cards       []AnomalousCard `json:"cards"`
}

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
	SpaceRatioThreshold float64 // kmeans cluster ratio threshold (score = cluster mean / baseline mean), default 2.0; max direction flags score > threshold, min direction flags score < 1/threshold

	// Debug
	EnableDebug bool // --debug-output: include all cards × all metrics in the result
}

// DefaultDetectionConfig returns a DetectionConfig with sensible defaults.
func DefaultDetectionConfig() DetectionConfig {
	return DetectionConfig{
		AggregationWindowSec: 10,
		TrimRatio:            0.25,
		MinSamplesForTrim:    4,

		SpaceRatioThreshold: 2.0,
	}
}

// =============================================================================
// Detection Result (top-level)
// =============================================================================

// DetectionResult is the complete KPI detection output. Anomalies are grouped
// by metric (metric-first), not by card.
type DetectionResult struct {
	Summary DetectionSummary `json:"summary"`
	Metrics []MetricAnomaly  `json:"anomaly_metrics,omitempty"`
	// Debug marks --debug-output mode (not serialized): in debug every card
	// appears in anomaly_metrics with the Abnormal flag; otherwise only
	// anomalous cards are listed and the flag is omitted.
	Debug bool `json:"-"`
}

// DetectionSummary is the overview section of the output.
type DetectionSummary struct {
	TotalCards          int     `json:"total_cards"`
	TotalNodes          int     `json:"total_nodes"`
	Anomalies           int     `json:"anomalies"`
	Normal              int     `json:"normal"`
	Source              string  `json:"source"`
	DataPoints          int     `json:"data_points"`
	SpaceRatioThreshold float64 `json:"space_ratio_threshold"`
}

// SpaceDetectionResult holds per-time-point space anomaly scores plus the
// flagged decision (parallel to Scores; cluster method uses the recursive
// Detect flag, absolute uses the sentinel).
type SpaceDetectionResult struct {
	Scores  map[int]map[MetricName][]float64
	Flagged map[int]map[MetricName][]bool
}

// =============================================================================
// Helpers
// =============================================================================

// HasAnomaly reports whether the result contains any anomalous card.
func HasAnomaly(result *DetectionResult) bool {
	return result != nil && result.Summary.Anomalies > 0
}
