package resource

import (
	"fmt"
	"os"
	"path/filepath"
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
	fmt.Fprintf(os.Stderr, "[SLOWNODE ALGO] Step 1/6: Parsing CSV...\n")
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
	fmt.Fprintf(os.Stderr, "[SLOWNODE ALGO] Step 1/6: Parsing KPI directory...\n")
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

// runDetection executes steps 2-6 on already-parsed TimeSeriesData.
//
// Pipeline:
//  2. AggregateByMinute
//  3. Space detection (peer comparison) on the last aggregated point
//  4. FuseAndSummarize (compute-first ordering)
//  5. BoundRootCause
//  6. CrossCardCorrelation
func runDetection(rawData *TimeSeriesData, source string, cfg DetectionConfig) (*DetectionResult, error) {
	// 2. Aggregate by the aggregation window (AggregationWindowSec, default 10s).
	fmt.Fprintf(os.Stderr, "[SLOWNODE ALGO] Step 2/6: Aggregating (trimmed mean, window=%ds)...\n", cfg.AggregationWindowSec)
	aggregated, err := AggregateByMinute(rawData.RawRows, rawData.CardIDs, cfg)
	if err != nil {
		return nil, fmt.Errorf("aggregate: %w", err)
	}
	fmt.Fprintf(os.Stderr, "[SLOWNODE ALGO] Aggregated to %d rows\n", len(aggregated))

	rawData.Rows = aggregated

	// 3. Space detection (peer comparison within each node, last point only).
	//    With the time dimension and baseline/detection windows removed, the
	//    judgment is purely the peer comparison on the most recent reading.
	fmt.Fprintf(os.Stderr, "[SLOWNODE ALGO] Step 3/6: Space (peer) detection...\n")
	spaceResult := detectSpaceAnomalies(aggregated, rawData.CardIDs, cfg, rawData.NodeOf)
	spaceDetails := aggregateSpaceScores(spaceResult, rawData.CardIDs, cfg)

	// 4. Fuse + compute-first ordering.
	fmt.Fprintf(os.Stderr, "[SLOWNODE ALGO] Step 4/6: Fusion (compute-first)...\n")
	summaries := FuseAndSummarize(spaceDetails, rawData.CardIDs, cfg)

	// 5. Root cause.
	fmt.Fprintf(os.Stderr, "[SLOWNODE ALGO] Step 5/6: Root-cause bounding...\n")
	rootCauses := BoundRootCause(summaries)

	// 6. Cross-card correlation.
	fmt.Fprintf(os.Stderr, "[SLOWNODE ALGO] Step 6/6: Cross-card correlation...\n")
	correlations := CrossCardCorrelation(rootCauses, summaries)

	// Node identity at the output boundary: convert global card IDs to per-node
	// IDs and attach the node name. Runs after root cause/correlation so those
	// stages keep using global IDs.
	summaries, rootCauses = applyNodeIdentity(summaries, rootCauses, rawData.NodeOf, rawData.LocalID)

	// Build result.
	confirmed := 0
	earlyDeg := 0
	indiv := 0
	normal := 0
	for _, s := range summaries {
		switch s.Quadrant {
		case QuadConfirmedAnomaly:
			confirmed++
		case QuadEarlyDegradation:
			earlyDeg++
		case QuadIndividualVariance:
			indiv++
		default:
			normal++
		}
	}

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
			TotalCards:         len(rawData.CardIDs),
			TotalNodes:         len(nodeSet),
			ConfirmedAnomalies: confirmed,
			EarlyDegradation:   earlyDeg,
			IndividualVariance: indiv,
			Normal:             normal,
			KPICSV:             source,
			TotalTimePoints:    len(aggregated),
		},
		Results:      summaries,
		RootCauses:   rootCauses,
		Correlations: correlations,
	}

	fmt.Fprintf(os.Stderr, "[SLOWNODE ALGO] KPI detection complete: confirmed=%d early=%d variance=%d normal=%d\n",
		confirmed, earlyDeg, indiv, normal)

	return result, nil
}

// applyNodeIdentity converts global card IDs to per-node IDs and attaches the
// node name at the output boundary. It is a pure bijection (LocalID), so scores,
// quadrants and ordering are untouched — only the reported identity changes.
func applyNodeIdentity(
	summaries []CardDetectionSummary,
	rootCauses []RootCauseResult,
	nodeOf map[int]string,
	localID map[int]int,
) ([]CardDetectionSummary, []RootCauseResult) {
	for i := range summaries {
		g := summaries[i].CardID
		summaries[i].Node = nodeOf[g]
		if summaries[i].Node == "" {
			summaries[i].Node = noneNode
		}
		summaries[i].CardID = localID[g]
	}
	for i := range rootCauses {
		g := rootCauses[i].CardID
		rootCauses[i].Node = nodeOf[g]
		if rootCauses[i].Node == "" {
			rootCauses[i].Node = noneNode
		}
		rootCauses[i].CardID = localID[g]
	}
	return summaries, rootCauses
}

// =============================================================================
// Text Report
// =============================================================================

// WriteReport generates a human-readable text report.
func WriteReport(result *DetectionResult, outputDir string) (string, error) {
	var b strings.Builder

	b.WriteString("================================================================================\n")
	b.WriteString("  NPU 资源 KPI 异常检测报告\n")
	b.WriteString("================================================================================\n\n")

	// Summary.
	b.WriteString("[SUMMARY]\n")
	fmt.Fprintf(&b, "  CSV:        %s\n", result.Summary.KPICSV)
	fmt.Fprintf(&b, "  数据点:     %d\n", result.Summary.TotalTimePoints)
	fmt.Fprintf(&b, "  总卡数:     %d\n", result.Summary.TotalCards)
	fmt.Fprintf(&b, "  ✓ 正常:     %d\n", result.Summary.Normal)
	fmt.Fprintf(&b, "  ✗ 确认异常: %d\n", result.Summary.ConfirmedAnomalies)
	fmt.Fprintf(&b, "  ⚡ 早期劣化: %d\n", result.Summary.EarlyDegradation)
	fmt.Fprintf(&b, "  ◇ 个体差异: %d\n", result.Summary.IndividualVariance)
	b.WriteString("\n")

	// Confirmed anomalies.
	if len(result.RootCauses) > 0 {
		b.WriteString("================================================================================\n")
		b.WriteString("  确认异常详情\n")
		b.WriteString("================================================================================\n\n")

		for _, rc := range result.RootCauses {
			fmt.Fprintf(&b, "  Node %s Card %d | %s | 置信度: %s\n", rc.Node, rc.CardID, rc.Category, rc.Confidence)
			fmt.Fprintf(&b, "  建议: %s\n", rc.Suggestion)
			if len(rc.Evidence) > 0 {
				b.WriteString("  异常指标:\n")
				for _, e := range rc.Evidence {
					fmt.Fprintf(&b, "    %-20s space=%.1f quadrant=%s\n",
						e.Metric, e.SpaceScore, e.Quadrant)
				}
			}
			b.WriteString("\n")
		}
	}

	// Correlations.
	if len(result.Correlations) > 0 {
		b.WriteString("================================================================================\n")
		b.WriteString("  跨卡关联分析\n")
		b.WriteString("================================================================================\n\n")
		for _, c := range result.Correlations {
			fmt.Fprintf(&b, "  [%s] %s (置信度: %s)\n", c.Type, c.Description, c.Confidence)
			fmt.Fprintf(&b, "  涉及卡: %v\n\n", c.CardIDs)
		}
	}

	b.WriteString("================================================================================\n")
	b.WriteString("  报告结束\n")
	b.WriteString("================================================================================\n")

	reportContent := b.String()

	// Write to file.
	outPath := filepath.Join(outputDir, "npu_resource_detection_report.log")
	if err := os.MkdirAll(outputDir, 0755); err != nil {
		return reportContent, fmt.Errorf("create output dir: %w", err)
	}
	if err := os.WriteFile(outPath, []byte(reportContent), 0644); err != nil {
		return reportContent, fmt.Errorf("write report: %w", err)
	}
	fmt.Fprintf(os.Stderr, "[SLOWNODE ALGO] KPI report written to %s\n", outPath)

	return reportContent, nil
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
