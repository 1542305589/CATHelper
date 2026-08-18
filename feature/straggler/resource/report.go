package resource

import (
	"fmt"
	"os"
	"strings"
)

// =============================================================================
// Detection Pipeline (orchestrator)
// =============================================================================

// RunDetection executes the full KPI detection pipeline for a CSV input and
// returns the result. ParseCSV → runDetection.
func RunDetection(csvPath string, cfg DetectionConfig) (*DetectionResult, error) {
	fmt.Fprintf(os.Stderr, "[SLOWNODE ALGO] KPI detection starting: %s\n", csvPath)

	// 1. Parse CSV.
	fmt.Fprintf(os.Stderr, "[SLOWNODE ALGO] Step 1/4: Parsing CSV...\n")
	rawData, err := ParseCSV(csvPath)
	if err != nil {
		return nil, fmt.Errorf("parse CSV: %w", err)
	}
	fmt.Fprintf(os.Stderr, "[SLOWNODE ALGO] Parsed %d raw rows, %d cards\n",
		len(rawData.Rows), len(rawData.CardIDs))

	return runDetection(rawData, csvPath, cfg)
}

// RunDetectionFromDir runs the KPI detection pipeline on a directory of
// per-node CSV files plus a fixed node_config.json (see ParseKPIDir). `dir`
// labels the input in the report summary.
func RunDetectionFromDir(dir string, cfg DetectionConfig) (*DetectionResult, error) {
	fmt.Fprintf(os.Stderr, "[SLOWNODE ALGO] KPI detection starting: %s\n", dir)

	// 1. Parse the directory (multi-node CSVs + node_config.json).
	fmt.Fprintf(os.Stderr, "[SLOWNODE ALGO] Step 1/4: Parsing KPI directory...\n")
	ts, err := ParseKPIDir(dir)
	if err != nil {
		return nil, fmt.Errorf("parse KPI dir: %w", err)
	}
	fmt.Fprintf(os.Stderr, "[SLOWNODE ALGO] Parsed %d rows, %d cards across %d nodes\n",
		len(ts.Rows), len(ts.CardIDs), uniqueNodes(ts.NodeOf))

	return runDetection(ts, dir, cfg)
}

// RunDetectionFromData runs the KPI detection pipeline on a pre-parsed
// TimeSeriesData (e.g. produced by ReadKPIFiles from CATMonitor's
// straggler_output JSONL). `source` labels the input in the report summary.
func RunDetectionFromData(ts *TimeSeriesData, source string, cfg DetectionConfig) (*DetectionResult, error) {
	fmt.Fprintf(os.Stderr, "[SLOWNODE ALGO] KPI detection starting (KPI file): %s\n", source)
	fmt.Fprintf(os.Stderr, "[SLOWNODE ALGO] Loaded %d raw rows, %d cards\n", len(ts.Rows), len(ts.CardIDs))
	return runDetection(ts, source, cfg)
}

// runDetection executes steps 2-4 on already-parsed TimeSeriesData.
//
// Pipeline:
//  2. AggregateByMinute
//  3. Space detection (peer comparison) on the last aggregated point
//  4. Build the metric-first anomaly list
func runDetection(rawData *TimeSeriesData, source string, cfg DetectionConfig) (*DetectionResult, error) {
	// 2. Aggregate by the aggregation window (AggregationWindowSec, default 10s).
	fmt.Fprintf(os.Stderr, "[SLOWNODE ALGO] Step 2/4: Aggregating (trimmed mean, window=%ds)...\n", cfg.AggregationWindowSec)
	aggregated, err := AggregateByMinute(rawData.RawRows, rawData.CardIDs, cfg)
	if err != nil {
		return nil, fmt.Errorf("aggregate: %w", err)
	}
	fmt.Fprintf(os.Stderr, "[SLOWNODE ALGO] Aggregated to %d rows\n", len(aggregated))

	rawData.Rows = aggregated

	// 3. Space detection (peer comparison within each node, last point only).
	fmt.Fprintf(os.Stderr, "[SLOWNODE ALGO] Step 3/4: Space (peer) detection...\n")
	spaceResult := detectSpaceAnomalies(aggregated, rawData.CardIDs, cfg, rawData.NodeOf)
	spaceDetails := aggregateScores(spaceResult, rawData.CardIDs, cfg)

	// 4. Build the metric-first anomaly list.
	fmt.Fprintf(os.Stderr, "[SLOWNODE ALGO] Step 4/4: Building metric-first anomalies...\n")
	metrics, anomalies := buildAnomalyMetrics(spaceDetails, rawData.CardIDs, rawData.NodeOf, rawData.LocalID, cfg)

	// Distinct node count (flat/single-node input → 1).
	nodeSet := make(map[string]bool)
	for _, n := range rawData.NodeOf {
		nodeSet[n] = true
	}
	if len(nodeSet) == 0 {
		nodeSet[noneNode] = true
	}

	result := &DetectionResult{
		Summary: DetectionSummary{
			TotalCards:          len(rawData.CardIDs),
			TotalNodes:          len(nodeSet),
			Anomalies:           anomalies,
			Normal:              len(rawData.CardIDs) - anomalies,
			Source:              source,
			DataPoints:          len(aggregated),
			SpaceRatioThreshold: cfg.SpaceRatioThreshold,
		},
		Metrics: metrics,
		Debug:   cfg.EnableDebug,
	}

	fmt.Fprintf(os.Stderr, "[SLOWNODE ALGO] KPI detection complete: anomalies=%d normal=%d\n",
		anomalies, result.Summary.Normal)

	return result, nil
}

// buildAnomalyMetrics groups the space-abnormal cards by metric (metric-first
// output) and counts how many distinct cards have at least one anomalous metric.
// With cfg.EnableDebug every card's space score is included per metric (normal
// ≈ 1.0) so undetected cards can be inspected; otherwise only anomalous cards.
func buildAnomalyMetrics(
	spaceDetails map[int]map[MetricName]*MetricAnomalyDetail,
	cardIDs []int,
	nodeOf map[int]string,
	localID map[int]int,
	cfg DetectionConfig,
) (metrics []MetricAnomaly, anomalies int) {
	anomalyCard := make(map[int]bool, len(cardIDs))

	for _, metric := range AllMetrics {
		ma := MetricAnomaly{
			Metric: metric,
			Method: MetricMetaRegistry[metric].Method,
		}
		for _, cid := range cardIDs {
			d := spaceDetails[cid][metric]
			if d == nil {
				continue
			}
			if !cfg.EnableDebug && !d.Abnormal {
				continue
			}
			node := nodeOf[cid]
			if node == "" {
				node = noneNode
			}
			c := AnomalousCard{Node: node, CardID: localID[cid], Score: d.Score}
			if cfg.EnableDebug {
				// Debug output lists every card, so carry the flag to
				// distinguish normal from abnormal (non-debug only lists
				// abnormal cards, so the flag is omitted).
				c.Abnormal = d.Abnormal
			}
			ma.Cards = append(ma.Cards, c)
			if d.Abnormal {
				anomalyCard[cid] = true
			}
		}
		if len(ma.Cards) > 0 {
			metrics = append(metrics, ma)
		}
	}

	return metrics, len(anomalyCard)
}

// =============================================================================
// Text Report
// =============================================================================

// WriteReport renders the human-readable KPI text report (stdout only — no
// file is written). The anomaly list is metric-first: each metric is followed
// by the cards anomalous for it with the space degradation degree in
// parentheses.
func WriteReport(result *DetectionResult) string {
	var b strings.Builder

	b.WriteString("================================================================================\n")
	b.WriteString("  NPU 资源 KPI 异常检测报告\n")
	b.WriteString("================================================================================\n\n")

	// Summary.
	b.WriteString("[SUMMARY]\n")
	fmt.Fprintf(&b, "  输入:       %s\n", result.Summary.Source)
	fmt.Fprintf(&b, "  数据点:     %d\n", result.Summary.DataPoints)
	fmt.Fprintf(&b, "  总卡数:     %d\n", result.Summary.TotalCards)
	fmt.Fprintf(&b, "  ✓ 正常:     %d\n", result.Summary.Normal)
	fmt.Fprintf(&b, "  ✗ 异常:     %d\n", result.Summary.Anomalies)
	b.WriteString("\n")

	// Metric-first anomaly list. Only anomalous cards are shown: in debug mode
	// every card is listed in the result with its Abnormal flag; otherwise only
	// anomalous cards are listed at all.
	printed := false
	for _, m := range result.Metrics {
		parts := make([]string, 0, len(m.Cards))
		for _, c := range m.Cards {
			if !c.Abnormal && result.Debug {
				continue
			}
			parts = append(parts, fmt.Sprintf("node%s:card%d(%.2f)", c.Node, c.CardID, c.Score))
		}
	}

	// Early degradation (watch list).
	earlyCards := filterByQuadrant(result.Results, QuadEarlyDegradation)
	if len(earlyCards) > 0 {
		b.WriteString("================================================================================\n")
		b.WriteString("  早期劣化（关注列表）\n")
		b.WriteString("================================================================================\n\n")
		for _, s := range earlyCards {
			fmt.Fprintf(&b, "  Card %d | score=%.2f\n", s.CardID, s.CompositeScore)
			for _, d := range s.AnomalyDetails {
				if d.TimeAbnormal {
					fmt.Fprintf(&b, "    %-20s time_z=%.1f current=%.1f baseline=%.1f±%.1f\n",
						d.Metric, d.TimeScore, d.CurrentMean, d.BaselineMean, d.BaselineStd)
				}
			}
			if len(s.TrendFindings) > 0 {
				for _, t := range s.TrendFindings {
					fmt.Fprintf(&b, "    [趋势] %s (R²=%.2f)\n", t.Desc, t.RSquared)
				}
			}
			b.WriteString("\n")
		}
		if !printed {
			b.WriteString("================================================================================\n")
			b.WriteString("  异常指标详情\n")
			b.WriteString("================================================================================\n\n")
			printed = true
		}
		fmt.Fprintf(&b, "  %-20s %s\n", m.Metric, strings.Join(parts, ", "))
	}

	b.WriteString("================================================================================\n")
	b.WriteString("  报告结束\n")
	b.WriteString("================================================================================\n")

	return b.String()
}

// =============================================================================
// Helpers
// =============================================================================

// uniqueNodes counts the distinct node names in a nodeOf mapping.
func uniqueNodes(nodeOf map[int]string) int {
	seen := make(map[string]bool)
	for _, n := range nodeOf {
		seen[n] = true
	}
	return len(seen)
}
