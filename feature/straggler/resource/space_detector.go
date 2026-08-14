package resource

import (
	"sort"

	"github.com/Computing-Availability-Tools/CATHelper/feature/straggler/clustering"
)

// =============================================================================
// Space (Peer-Comparison) Dimension Detection
// =============================================================================

// detectSpaceAnomalies computes space scores for all cards across all metrics
// using ONLY the last aggregated point of the detection window (the most recent
// reading is what matters for a straggler).
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
		Scores:  make(map[int]map[MetricName][]float64),
		Flagged: make(map[int]map[MetricName][]bool),
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
		result.Flagged[cid] = make(map[MetricName][]bool, len(AllMetrics))
		for _, metric := range AllMetrics {
			result.Scores[cid][metric] = make([]float64, 0, 1)
			result.Flagged[cid][metric] = make([]bool, 0, 1)
		}
	}

	// Only the last aggregated point is judged.
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
					result.Flagged[cid][metric] = append(result.Flagged[cid][metric], false)
				}
				continue
			}

			switch meta.Method {
			case MethodAbsolute:
				// Absolute threshold: > threshold → anomaly.
				for _, cid := range nodeCardIDs {
					z := 0.0
					f := false
					if v, ok := dict[cid]; ok && v > meta.AbsThreshold {
						z = 999 // sentinel for "absolute anomaly"
						f = true
					}
					result.Scores[cid][metric] = append(result.Scores[cid][metric], z)
					result.Flagged[cid][metric] = append(result.Flagged[cid][metric], f)
				}

			case MethodCluster:
				// kmeans ratio detection within THIS node on the last point.
				// Only present values > 0 participate. The anomaly direction is
				// NOT pre-decided: both directions are run and the flagged COUNT
				// picks the anomaly side.
				//
				//   α1 = max-direction flags (baseline = min-mean cluster,
				//        flags the clusters above it beyond the ratio threshold)
				//   α2 = min-direction flags (baseline = max-mean cluster,
				//        flags the clusters below it beyond the threshold)
				//
				// The side flagging FEWER cards is the minority = the anomaly:
				// a single card deviating from the majority mode is a straggler,
				// a majority deviating is just the normal mode. Equal counts
				// (incl. 0 == 0 healthy) → nothing flagged.
				//
				// Every participating card gets its REAL top-level cluster ratio
				// from the winning direction's Diagnose: baseline members are
				// exactly 1.0, other non-flagged clusters keep their real ratio
				// (e.g. 1.2), flagged cards their ratio (> threshold). The FLAG
				// comes from the recursive Detect decision; Diagnose only fills
				// the score. Absent / non-positive cards stay 0 — no ratio.
				posPresent, posVals := filterPositive(present, presentVals)
				if len(posVals) < 2 {
					for _, cid := range nodeCardIDs {
						result.Scores[cid][metric] = append(result.Scores[cid][metric], 0)
						result.Flagged[cid][metric] = append(result.Flagged[cid][metric], false)
					}
					continue
				}
				resMax := clustering.Detect(posVals, cfg.SpaceRatioThreshold, true)  // α1
				resMin := clustering.Detect(posVals, cfg.SpaceRatioThreshold, false) // α2
				winner := resMax
				highWins := true // scores always come from the max run on a tie
				if len(resMin) < len(resMax) {
					winner, highWins = resMin, false
				} else if len(resMin) == len(resMax) {
					winner = nil // tie (incl. 0 == 0) → nothing flagged
				}
				flagged := make(map[int]bool, len(winner))
				for _, r := range winner {
					flagged[nodeCardIDs[posPresent[r.Index]]] = true
				}
				ratioOf := make(map[int]float64, len(posPresent))
				for _, e := range clustering.Diagnose(posVals, cfg.SpaceRatioThreshold, highWins) {
					ratioOf[nodeCardIDs[posPresent[e.Index]]] = e.Ratio
				}
				for _, cid := range nodeCardIDs {
					result.Scores[cid][metric] = append(result.Scores[cid][metric], ratioOf[cid])
					result.Flagged[cid][metric] = append(result.Flagged[cid][metric], flagged[cid])
				}

			default: // unknown method → no score
				for _, cid := range nodeCardIDs {
					result.Scores[cid][metric] = append(result.Scores[cid][metric], 0)
					result.Flagged[cid][metric] = append(result.Flagged[cid][metric], false)
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

// aggregateScores reduces per-time-point space scores to per-card
// aggregate space scores. With the last-point-only space detection, the score
// array holds exactly one element.
func aggregateScores(space *SpaceDetectionResult, cardIDs []int, cfg DetectionConfig) map[int]map[MetricName]*MetricAnomalyDetail {
	result := make(map[int]map[MetricName]*MetricAnomalyDetail)

	for _, cid := range cardIDs {
		result[cid] = make(map[MetricName]*MetricAnomalyDetail)
		for _, metric := range AllMetrics {
			zscores := space.Scores[cid][metric]
			if len(zscores) == 0 {
				result[cid][metric] = &MetricAnomalyDetail{
					Metric: metric,
					Score:  0,
				}
				continue
			}
			flags := space.Flagged[cid][metric]

			// Absolute methods flag via the 999 sentinel; cluster methods score
			// the real top-level cluster ratio.
			meta := MetricMetaRegistry[metric]
			isSentinel := meta.Method == MethodAbsolute

			var sum float64
			flaggedCount := 0
			for i, z := range zscores {
				sum += z
				if i < len(flags) && flags[i] {
					flaggedCount++
				}
			}

			var score float64
			var abnormal bool
			if isSentinel {
				// Absolute: score = fraction of flagged points; abnormal if >50%.
				score = float64(flaggedCount) / float64(len(zscores))
				abnormal = score > 0.5
			} else {
				// Cluster: score = the real cluster ratio (baseline members are
				// exactly 1.0, other non-flagged clusters keep their real ratio
				// e.g. 1.2); abnormal follows the recursive Detect flag, not the
				// raw ratio.
				score = sum / float64(len(zscores))
				abnormal = float64(flaggedCount)/float64(len(zscores)) > 0.5
			}

			result[cid][metric] = &MetricAnomalyDetail{
				Metric:   metric,
				Score:    score,
				Abnormal: abnormal,
			}
		}
	}

	return result
}
