package resource

import (
	"math"
	"sort"

	"github.com/Computing-Availability-Tools/CATHelper/feature/straggler/clustering"
)

// =============================================================================
// Space (Peer-Comparison) Dimension Detection
// =============================================================================

// detectSpaceAnomalies computes space scores for all cards across all metrics
// using ONLY the last aggregated minute point of the detection window (the
// most recent reading is what matters for a straggler).
//
// Returns: cardID → metric → []score (exactly one element — the last point).
// nodeOf is optional; when provided, peer comparison happens WITHIN each node
// (cards in different nodes are never compared). Omitted → single "none" node.
func detectSpaceAnomalies(
	detectionRows []CSVRow,
	cardIDs []int,
	cfg DetectionConfig,
	nodeOf ...map[int]string,
) *SpaceDetectionResult {
	result := &SpaceDetectionResult{
		Scores: make(map[int]map[MetricName][]float64),
	}

	// Group cards by node. nodes is a partition of cardIDs, so every card is
	// processed exactly once per (metric).
	var nodes map[string][]int
	if len(nodeOf) > 0 {
		nodes = buildNodeGroups(cardIDs, nodeOf[0])
	} else {
		nodes = map[string][]int{noneNode: append([]int(nil), cardIDs...)}
	}
	nodeList := sortedNodeKeys(nodes)

	// Init per-card score slices (exactly 1 slot: the last time point).
	for _, cid := range cardIDs {
		result.Scores[cid] = make(map[MetricName][]float64, len(AllMetrics))
		for _, metric := range AllMetrics {
			result.Scores[cid][metric] = make([]float64, 0, 1)
		}
	}

	// Only the last aggregated minute point is judged.
	if len(detectionRows) == 0 {
		return result
	}
	row := detectionRows[len(detectionRows)-1]

	// For each metric, for each node: peer comparison happens WITHIN the node
	// only, on the single last time point.
	for _, metric := range AllMetrics {
		meta := MetricMetaRegistry[metric]
		dict := getMetricDict(row, metric)

		for _, node := range nodeList {
			nodeCardIDs := nodes[node]
			vals := getMetricValues(row, metric, nodeCardIDs)
			present, presentVals := filterPresent(dict, nodeCardIDs, vals)

			if len(presentVals) < 2 {
				// Need at least 2 cards present IN THIS NODE for peer comparison.
				for _, cid := range nodeCardIDs {
					result.Scores[cid][metric] = append(result.Scores[cid][metric], 0)
				}
				continue
			}

			switch meta.SpaceMethod {
			case MethodAbsolute:
				// Absolute threshold: > threshold → anomaly.
				for _, cid := range nodeCardIDs {
					z := 0.0
					if v, ok := dict[cid]; ok && v > meta.AbsThreshold {
						z = 999 // sentinel for "absolute anomaly"
					}
					result.Scores[cid][metric] = append(result.Scores[cid][metric], z)
				}

			case MethodIQR:
				sorted := make([]float64, len(presentVals))
				copy(sorted, presentVals)
				sort.Float64s(sorted)
				q1 := Percentile(sorted, 0.25)
				q3 := Percentile(sorted, 0.75)
				iqr := q3 - q1
				lower := q1 - cfg.SpaceIQRMult*iqr
				upper := q3 + cfg.SpaceIQRMult*iqr
				for _, cid := range nodeCardIDs {
					z := 0.0
					if v, ok := dict[cid]; ok && (v < lower || v > upper) {
						z = 999
					}
					result.Scores[cid][metric] = append(result.Scores[cid][metric], z)
				}

			case MethodCluster:
				// kmeans ratio detection within THIS node on the last point.
				// Only present values > 0 participate; the ratio score =
				// deepest-cluster mean / baseline (direction extreme) cluster
				// mean.
				posPresent, posVals := filterPositive(present, presentVals)
				if len(posVals) < 2 {
					for _, cid := range nodeCardIDs {
						result.Scores[cid][metric] = append(result.Scores[cid][metric], 0)
					}
					continue
				}
				res := clustering.Detect(posVals, cfg.SpaceRatioThreshold, meta.Direction == DirHigh)
				flagged := make(map[int]float64, len(res))
				for _, r := range res {
					cid := nodeCardIDs[posPresent[r.Index]]
					flagged[cid] = r.Ratio
				}
				for _, cid := range nodeCardIDs {
					if ratio, ok := flagged[cid]; ok {
						result.Scores[cid][metric] = append(result.Scores[cid][metric], ratio)
					} else {
						result.Scores[cid][metric] = append(result.Scores[cid][metric], 0)
					}
				}

			default: // MethodZScore
				mean, std := MeanStd(presentVals)
				for _, cid := range nodeCardIDs {
					z := 0.0
					if v, ok := dict[cid]; ok && std > 0 {
						z = math.Abs(v-mean) / std
					}
					result.Scores[cid][metric] = append(result.Scores[cid][metric], z)
				}
			}
		}
	}

	return result
}

// buildNodeGroups partitions cardIDs by node name (defaulting missing cards to
// "none"). Each card appears in exactly one group.
func buildNodeGroups(cardIDs []int, nodeOf map[int]string) map[string][]int {
	nodes := make(map[string][]int)
	for _, cid := range cardIDs {
		n := nodeOf[cid]
		if n == "" {
			n = noneNode
		}
		nodes[n] = append(nodes[n], cid)
	}
	return nodes
}

// sortedNodeKeys returns the node names sorted for deterministic iteration.
func sortedNodeKeys(nodes map[string][]int) []string {
	keys := make([]string, 0, len(nodes))
	for k := range nodes {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// filterPresent returns the indices (into nodeCardIDs) of cards present in
// dict, along with their values. Absent cards are excluded so they are never
// scored (their getMetricValues slot is 0).
func filterPresent(dict map[int]float64, nodeCardIDs []int, vals []float64) (present []int, presentVals []float64) {
	for i, cid := range nodeCardIDs {
		if _, ok := dict[cid]; ok {
			present = append(present, i)
			presentVals = append(presentVals, vals[i])
		}
	}
	return present, presentVals
}

// filterPositive narrows present entries to values > 0 (kmeans ignores idle /
// zero readings), keeping indices into nodeCardIDs.
func filterPositive(present []int, presentVals []float64) (posPresent []int, posVals []float64) {
	for i, v := range presentVals {
		if v > 0 {
			posPresent = append(posPresent, present[i])
			posVals = append(posVals, v)
		}
	}
	return posPresent, posVals
}

// =============================================================================
// Space Score Aggregation
// =============================================================================

// aggregateSpaceScores reduces per-time-point space scores to per-card
// aggregate space scores. With the last-point-only space detection, the score
// array holds exactly one element.
func aggregateSpaceScores(space *SpaceDetectionResult, cardIDs []int, cfg DetectionConfig) map[int]map[MetricName]*MetricAnomalyDetail {
	result := make(map[int]map[MetricName]*MetricAnomalyDetail)

	for _, cid := range cardIDs {
		result[cid] = make(map[MetricName]*MetricAnomalyDetail)
		for _, metric := range AllMetrics {
			zscores := space.Scores[cid][metric]
			if len(zscores) == 0 {
				result[cid][metric] = &MetricAnomalyDetail{
					Metric:     metric,
					SpaceScore: 0,
				}
				continue
			}

			// For absolute methods, consider "abnormal" if any point had a
			// sentinel value.
			meta := MetricMetaRegistry[metric]
			isSentinel := meta.SpaceMethod == MethodAbsolute
			isCluster := meta.SpaceMethod == MethodCluster

			var sum float64
			abnormalCount := 0
			for _, z := range zscores {
				if isSentinel {
					if z >= 999 {
						abnormalCount++
					}
				} else {
					sum += z
					if z > cfg.SpaceZThreshold {
						abnormalCount++
					}
				}
			}

			var spaceScore float64
			var spaceAbnormal bool
			switch {
			case isSentinel:
				// For absolute/direct: abnormal if >50% of points flagged.
				spaceScore = float64(abnormalCount) / float64(len(zscores))
				spaceAbnormal = spaceScore > 0.5
			case isCluster:
				// Cluster method: space_score is the deepest-cluster ratio
				// (cluster mean / baseline mean) on the last point. Abnormal
				// when the ratio exceeds the ratio threshold. The kmeans
				// algorithm has no historical baseline mean or noise scale, so
				// SpaceRef (space_baseline_mean) and SpaceScale (space_scale)
				// stay 0.
				spaceScore = sum / float64(len(zscores))
				spaceAbnormal = spaceScore > cfg.SpaceRatioThreshold
			default:
				spaceScore = sum / float64(len(zscores))
				spaceAbnormal = spaceScore > cfg.SpaceZThreshold
			}

			result[cid][metric] = &MetricAnomalyDetail{
				Metric:        metric,
				SpaceScore:    spaceScore,
				SpaceAbnormal: spaceAbnormal,
			}
		}
	}

	return result
}
