package resource

import (
	"fmt"
	"math"
)

// =============================================================================
// Time (Self-Comparison) Dimension Detection
// =============================================================================

// detectTimeAnomalies compares each card's current (detection-window) values
// against its own historical baseline.
//
// Returns: cardID → metric → time anomaly score + flags.
func detectTimeAnomalies(
	detectionRows []CSVRow,
	baselines map[int]map[MetricName]*CardBaseline,
	cardIDs []int,
	cfg DetectionConfig,
) *TimeDetectionResult {
	result := &TimeDetectionResult{
		Scores: make(map[int]map[MetricName]float64),
	}

	for _, cid := range cardIDs {
		result.Scores[cid] = make(map[MetricName]float64)
		for _, metric := range AllMetrics {
			baseline := baselines[cid][metric]
			// Baseline too small (< MinBaselineSamples, e.g. < 5 min of 10s
			// points) → no reliable self baseline → time Z=0, not judged.
			if baseline == nil || baseline.N < cfg.MinBaselineSamples {
				result.Scores[cid][metric] = 0
				continue
			}

			// Collect current values for this card+metric from detection window.
			var curVals []float64
			for _, row := range detectionRows {
				dict := getMetricDict(row, metric)
				if dict == nil {
					continue
				}
				if v, ok := dict[cid]; ok {
					curVals = append(curVals, v)
				}
			}

			if len(curVals) == 0 {
				result.Scores[cid][metric] = 0
				continue
			}

			// Direction-aware Z-Score: only deviations on the metric's anomaly
			// side count (DirHigh → current above baseline; DirLow → current
			// below baseline). Benign-side movement scores 0, so a DirHigh metric
			// dropping (or a DirLow metric rising) is never flagged.
			direction := MetricMetaRegistry[metric].Direction
			var zScore float64
			if MetricMetaRegistry[metric].TimeMethod == MethodMAD {
				// Robust time Z-Score using median + MAD. Median and MAD have a
				// 50% breakdown point, so historical fault episodes (minority of
				// baseline) cannot drag the baseline center or inflate the scale.
				currentMedian := Median(curVals)
				scale := madToStdFactor * baseline.Mad
				if scale > 0 {
					zScore = oneSidedZ(currentMedian, baseline.Median, scale, direction)
				} else {
					zScore = oneSidedSentinel(currentMedian, baseline.Median, direction)
				}
			} else {
				// Classic time Z-Score = (current - baseline_mean) / baseline_std,
				// also one-sided along the metric direction.
				var sum float64
				for _, v := range curVals {
					sum += v
				}
				currentMean := sum / float64(len(curVals))

				if baseline.StdDev > 0 {
					zScore = oneSidedZ(currentMean, baseline.Mean, baseline.StdDev, direction)
				} else {
					zScore = oneSidedSentinel(currentMean, baseline.Mean, direction)
				}
			}

			result.Scores[cid][metric] = zScore
		}
	}

	return result
}

// oneSidedZ is the direction-aware time Z-score: only the metric's anomaly side
// counts. DirHigh (higher is worse) scores only when current > baseline; DirLow
// scores only when current < baseline. Benign-side deviations return 0.
func oneSidedZ(current, baseline, scale float64, direction AnomalyDirection) float64 {
	switch direction {
	case DirLow:
		if current < baseline {
			return (baseline - current) / scale
		}
	default: // DirHigh
		if current > baseline {
			return (current - baseline) / scale
		}
	}
	return 0
}

// oneSidedSentinel is the direction-aware counterpart of the 999 sentinel used
// when the baseline scale is 0 (perfectly stable history): any movement on the
// anomaly side is 999, the benign side stays 0.
func oneSidedSentinel(current, baseline float64, direction AnomalyDirection) float64 {
	var diff float64
	switch direction {
	case DirLow:
		if current < baseline {
			diff = baseline - current
		}
	default: // DirHigh
		if current > baseline {
			diff = current - baseline
		}
	}
	if diff > 0.01 {
		return 999
	}
	return 0
}

// =============================================================================
// Time Score Aggregation
// =============================================================================

// aggregateTimeScores converts raw time Z-Scores into MetricAnomalyDetail,
// merging with the space details computed earlier.
// nodeOf is optional: when provided, the reported peer mean is scoped to the
// card's own node (cards in other nodes are not counted as peers).
func aggregateTimeScores(
	time *TimeDetectionResult,
	detectionRows []CSVRow,
	baselines map[int]map[MetricName]*CardBaseline,
	cardIDs []int,
	cfg DetectionConfig,
	nodeOf ...map[int]string,
) map[int]map[MetricName]*MetricAnomalyDetail {
	result := make(map[int]map[MetricName]*MetricAnomalyDetail)

	// nodeName resolves a card's node, defaulting to "none" (flat input).
	nodeName := func(g int) string {
		if len(nodeOf) > 0 {
			if n := nodeOf[0][g]; n != "" {
				return n
			}
		}
		return noneNode
	}

	for _, cid := range cardIDs {
		result[cid] = make(map[MetricName]*MetricAnomalyDetail)
		// Peers = other cards in cid's node (or all cards when nodeOf absent).
		var peerIDs []int
		for _, ocid := range cardIDs {
			if ocid == cid {
				continue
			}
			if nodeName(ocid) == nodeName(cid) {
				peerIDs = append(peerIDs, ocid)
			}
		}

		for _, metric := range AllMetrics {
			baseline := baselines[cid][metric]
			timeZ := time.Scores[cid][metric]
			timeAbnormal := false
			if timeZ >= 999 {
				timeAbnormal = true
			} else if timeZ > cfg.TimeZThreshold {
				timeAbnormal = true
			}

			// Compute current mean and peer mean for context.
			var curVals []float64
			for _, row := range detectionRows {
				dict := getMetricDict(row, metric)
				if dict == nil {
					continue
				}
				if v, ok := dict[cid]; ok {
					curVals = append(curVals, v)
				}
			}
			var currentMean float64
			if len(curVals) > 0 {
				var sum float64
				for _, v := range curVals {
					sum += v
				}
				currentMean = sum / float64(len(curVals))
			}
			// For robust metrics, report the robust center (median) as the
			// "current" value so it matches the Z-Score that produced the alarm.
			if MetricMetaRegistry[metric].TimeMethod == MethodMAD && len(curVals) > 0 {
				currentMean = Median(curVals)
			}

			// Peer mean across detection window (same node only).
			var peerVals []float64
			for _, row := range detectionRows {
				dict := getMetricDict(row, metric)
				if dict == nil {
					continue
				}
				for _, ocid := range peerIDs {
					if v, ok := dict[ocid]; ok {
						peerVals = append(peerVals, v)
					}
				}
			}
			var peerMean float64
			if len(peerVals) > 0 {
				var sum float64
				for _, v := range peerVals {
					sum += v
				}
				peerMean = sum / float64(len(peerVals))
			}

			bMean := 0.0
			bStd := 0.0
			if baseline != nil {
				if MetricMetaRegistry[metric].TimeMethod == MethodMAD {
					// Report the robust baseline: median ± robust sigma.
					bMean = baseline.Median
					bStd = madToStdFactor * baseline.Mad
				} else {
					bMean = baseline.Mean
					bStd = baseline.StdDev
				}
			}

			detail := &MetricAnomalyDetail{
				Metric:        metric,
				TimeScore:     timeZ,
				TimeAbnormal:  timeAbnormal,
				CurrentMean:   currentMean,
				BaselineMean:  bMean,
				BaselineStd:   bStd,
				PeerMean:      peerMean,
			}

			result[cid][metric] = detail
		}
	}

	return result
}

// =============================================================================
// Trend Detection
// =============================================================================

// detectTrends runs linear regression on each card's metric across the full
// time series (baseline + detection) to find sustained upward/downward trends.
func detectTrends(allRows []CSVRow, cardIDs []int, cfg DetectionConfig) map[int][]TrendFinding {
	if !cfg.EnableTrend {
		return nil
	}

	result := make(map[int][]TrendFinding)

	for _, cid := range cardIDs {
		for _, metric := range AllMetrics {
			// Collect (timestamp, value) pairs.
			var ts, vals []float64
			for _, row := range allRows {
				dict := getMetricDict(row, metric)
				if dict == nil {
					continue
				}
				if v, ok := dict[cid]; ok {
					ts = append(ts, float64(row.Timestamp))
					vals = append(vals, v)
				}
			}

			if len(ts) < 10 {
				continue
			}

			// Simple linear regression: y = slope*x + intercept.
			slope, rSquared := linearRegression(ts, vals)
			if rSquared >= cfg.TrendMinRSquared && slope != 0 {
				desc := formatTrendDesc(metric, slope)
				result[cid] = append(result[cid], TrendFinding{
					Metric:   metric,
					Slope:    slope,
					RSquared: rSquared,
					Desc:     desc,
				})
			}
		}
	}

	return result
}

// =============================================================================
// Linear Regression
// =============================================================================

// linearRegression computes slope and R² for y = slope*x + intercept.
func linearRegression(x, y []float64) (slope, rSquared float64) {
	n := float64(len(x))
	if n < 2 {
		return 0, 0
	}

	var sumX, sumY, sumXY, sumX2, sumY2 float64
	for i := range x {
		sumX += x[i]
		sumY += y[i]
		sumXY += x[i] * y[i]
		sumX2 += x[i] * x[i]
		sumY2 += y[i] * y[i]
	}

	denom := n*sumX2 - sumX*sumX
	if denom == 0 {
		return 0, 0
	}

	slope = (n*sumXY - sumX*sumY) / denom
	intercept := (sumY - slope*sumX) / n

	// R² = 1 - (SS_res / SS_tot).
	meanY := sumY / n
	var ssRes, ssTot float64
	for i := range x {
		pred := slope*x[i] + intercept
		ssRes += (y[i] - pred) * (y[i] - pred)
		ssTot += (y[i] - meanY) * (y[i] - meanY)
	}

	if ssTot == 0 {
		return slope, 1
	}
	rSquared = 1 - ssRes/ssTot
	if rSquared < 0 {
		rSquared = 0
	}

	return slope, rSquared
}

// formatTrendDesc produces a human-readable trend description.
func formatTrendDesc(metric MetricName, slope float64) string {
	// slope is value-per-second (since x is Unix timestamps).
	perMinute := slope * 60
	perHour := slope * 3600
	perDay := slope * 86400

	var val float64
	var unit string
	if math.Abs(perDay) >= 0.1 {
		val = perDay
		unit = "天"
	} else if math.Abs(perHour) >= 0.1 {
		val = perHour
		unit = "小时"
	} else {
		val = perMinute
		unit = "分钟"
	}

	dir := "上升"
	if val < 0 {
		dir = "下降"
		val = -val
	}

	return string(metric) + " 持续" + dir + ": " + formatFloat(val) + "/" + unit
}

func formatFloat(v float64) string {
	absV := v
	if absV < 0 {
		absV = -absV
	}
	if absV < 0.01 {
		return "≈0"
	}
	if absV < 1 {
		return fmt.Sprintf("%.3f", v)
	}
	if absV < 100 {
		return fmt.Sprintf("%.2f", v)
	}
	return fmt.Sprintf("%.1f", v)
}
