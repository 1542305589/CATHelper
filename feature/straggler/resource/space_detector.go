package resource

import (
	"math"
	"sort"
)

// =============================================================================
// Space (Peer-Comparison) Dimension Detection
// =============================================================================

// detectSpaceAnomalies computes per-time-point space scores for all cards
// across all metrics over the detection window.
//
// Returns: cardID → metric → []score (one per detection-window time point).
// nodeOf is optional; when provided, peer comparison happens WITHIN each node
// (cards in different nodes are never compared). Omitted → single "none" node.
func detectSpaceAnomalies(
	detectionRows []CSVRow,
	baselines map[int]map[MetricName]*CardBaseline,
	cardIDs []int,
	cfg DetectionConfig,
	nodeOf ...map[int]string,
) *SpaceDetectionResult {
	result := &SpaceDetectionResult{
		Scores:     make(map[int]map[MetricName][]float64),
		ClusterRef: make(map[int]map[MetricName][]float64),
		ScaleRef:   make(map[int]map[MetricName]float64),
	}

	// Group cards by node. nodes is a partition of cardIDs, so every card is
	// processed exactly once per (row, metric).
	var nodes map[string][]int
	if len(nodeOf) > 0 {
		nodes = buildNodeGroups(cardIDs, nodeOf[0])
	} else {
		nodes = map[string][]int{noneNode: append([]int(nil), cardIDs...)}
	}
	nodeList := sortedNodeKeys(nodes)

	// Per-node, per-metric robust noise scale, self-calibrated from the
	// historical baselines (median across the node's cards of 1.4826 × MAD).
	scale := make(map[string]map[MetricName]float64, len(nodeList))
	for _, node := range nodeList {
		scale[node] = make(map[MetricName]float64, len(AllMetrics))
		for _, metric := range AllMetrics {
			scale[node][metric] = spaceMetricScale(metric, baselines, nodes[node])
		}
	}

	// Init per-card score + cluster-ref + scale slices.
	for _, cid := range cardIDs {
		result.Scores[cid] = make(map[MetricName][]float64)
		result.ClusterRef[cid] = make(map[MetricName][]float64)
		result.ScaleRef[cid] = make(map[MetricName]float64)
		for _, metric := range AllMetrics {
			result.Scores[cid][metric] = make([]float64, 0, len(detectionRows))
			result.ClusterRef[cid][metric] = make([]float64, 0, len(detectionRows))
		}
	}

	// appendScore records one time-point score and (for cluster) the node's
	// baseline-cluster mean reference into the aligned arrays.
	appendScore := func(cid int, metric MetricName, z, ref float64) {
		result.Scores[cid][metric] = append(result.Scores[cid][metric], z)
		result.ClusterRef[cid][metric] = append(result.ClusterRef[cid][metric], ref)
	}

	// For each time point, for each metric, for each node: peer comparison
	// happens WITHIN the node only.
	for _, row := range detectionRows {
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
						appendScore(cid, metric, 0, 0)
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
						appendScore(cid, metric, z, 0)
					}

				case MethodDirect:
					// Direct comparison (for freq): a card is downclocked when it
					// runs at least FreqDownclockGap below its peers' minimum.
					// The peer minimum EXCLUDES the card itself — a single
					// downclocked card (the global min) is compared against the
					// second-smallest value. Cards absent at this time point are
					// never flagged.
					sorted := make([]float64, len(presentVals))
					copy(sorted, presentVals)
					sort.Float64s(sorted)
					globalMin := sorted[0]
					secondMin := sorted[1] // len(presentVals) >= 2 enforced above
					for _, cid := range nodeCardIDs {
						v, ok := dict[cid]
						if !ok {
							appendScore(cid, metric, 0, 0)
							continue
						}
						peerMin := globalMin
						if v <= globalMin {
							// Card is (tied for) the minimum → its lowest peer
							// is the second-smallest present value. A tie with
							// another card at the same low value is not a unique
							// downclock (secondMin equals globalMin then).
							peerMin = secondMin
						}
						z := 0.0
						if v < peerMin-cfg.FreqDownclockGap {
							z = 999
						}
						appendScore(cid, metric, z, 0)
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
						appendScore(cid, metric, z, 0)
					}

				case MethodCluster:
					// Majority-mode clustering within THIS node: full partition,
					// majority (baseline) cluster mean as the reference, baseline
					// members exempt, per-point one-sided z in the node's noise.
					zAtT := make(map[int]float64, len(nodeCardIDs))
					sortedIdx := make([]int, len(present))
					for k := range present {
						sortedIdx[k] = k // index into present/presentVals
					}
					sort.Slice(sortedIdx, func(a, b int) bool {
						return presentVals[sortedIdx[a]] < presentVals[sortedIdx[b]]
					})
					clusters := gapSplitClusters(sortedIdx, presentVals)
					baseIdx := pickBaselineCluster(clusters, presentVals, meta.Direction)
					baseMean := clusterMean(clusters[baseIdx], presentVals)

					baseMembers := make(map[int]bool, len(clusters[baseIdx]))
					for _, k := range clusters[baseIdx] {
						baseMembers[k] = true
					}

					for pi, pv := range presentVals {
						if baseMembers[pi] {
							continue // majority = the reference, not judged
						}
						// One-sided: only the anomaly direction is checked.
						if (meta.Direction == DirHigh && pv <= baseMean) ||
							(meta.Direction == DirLow && pv >= baseMean) {
							continue
						}
						z := math.Abs(pv-baseMean) / scale[node][metric]
						if z > cfg.SpaceClusterK {
							zAtT[nodeCardIDs[present[pi]]] = z
						}
					}
					for _, cid := range nodeCardIDs {
						appendScore(cid, metric, zAtT[cid], baseMean)
						result.ScaleRef[cid][metric] = scale[node][metric]
					}

				default: // MethodZScore
					mean, std := MeanStd(presentVals)
					for _, cid := range nodeCardIDs {
						z := 0.0
						if v, ok := dict[cid]; ok && std > 0 {
							z = math.Abs(v-mean) / std
						}
						appendScore(cid, metric, z, 0)
					}
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

// =============================================================================
// MethodCluster helpers
// =============================================================================

// gapSplitClusters recursively splits the sorted present values at the largest
// adjacent gap, on BOTH sides, until no sub-group has a gap ≥ half its own
// span. It returns a full partition of index lists (indices into the values
// slice). This mirrors the Profiler homogenization clustering but decomposes
// both sides so all anomaly levels are separated from the normal core.
func gapSplitClusters(sortedIdx []int, vals []float64) [][]int {
	if len(sortedIdx) <= 1 {
		return [][]int{sortedIdx}
	}
	maxGap := -1.0
	splitPos := -1
	for i := 0; i < len(sortedIdx)-1; i++ {
		g := vals[sortedIdx[i+1]] - vals[sortedIdx[i]]
		if g > maxGap {
			maxGap = g
			splitPos = i
		}
	}
	span := vals[sortedIdx[len(sortedIdx)-1]] - vals[sortedIdx[0]]
	if span <= 0 || maxGap*2 < span {
		// All identical (span 0) or no dominant gap → no structure → one cluster.
		return [][]int{sortedIdx}
	}
	left := gapSplitClusters(sortedIdx[:splitPos+1], vals)
	right := gapSplitClusters(sortedIdx[splitPos+1:], vals)
	return append(left, right...)
}

// clusterMean returns the mean value of a cluster (list of indices into vals).
func clusterMean(idxList []int, vals []float64) float64 {
	if len(idxList) == 0 {
		return 0
	}
	var sum float64
	for _, k := range idxList {
		sum += vals[k]
	}
	return sum / float64(len(idxList))
}

// pickBaselineCluster selects the baseline: the largest cluster (the peer
// majority). On a member-count tie, the direction extreme is preferred —
// DirHigh → lowest mean, DirLow → highest mean.
func pickBaselineCluster(clusters [][]int, vals []float64, dir AnomalyDirection) int {
	maxCount := 0
	for _, cl := range clusters {
		if len(cl) > maxCount {
			maxCount = len(cl)
		}
	}
	best := -1
	for i, cl := range clusters {
		if len(cl) != maxCount {
			continue
		}
		if best == -1 {
			best = i
			continue
		}
		bMean := clusterMean(clusters[best], vals)
		cMean := clusterMean(cl, vals)
		if (dir == DirHigh && cMean < bMean) || (dir == DirLow && cMean > bMean) {
			best = i
		}
	}
	return best
}

// spaceMetricScale returns the robust noise scale for a metric, self-calibrated
// from the historical baselines: the median across cards of 1.4826 × MAD.
// Cards with zero historical MAD (constant values, e.g. idle util) are skipped;
// if every card is constant, a tiny floor avoids division by zero.
func spaceMetricScale(
	metric MetricName,
	baselines map[int]map[MetricName]*CardBaseline,
	cardIDs []int,
) float64 {
	var nonZero []float64
	for _, cid := range cardIDs {
		bl := baselines[cid][metric]
		if bl == nil || bl.N < 2 {
			continue
		}
		s := madToStdFactor * bl.Mad
		if s > 0 {
			nonZero = append(nonZero, s)
		}
	}
	if len(nonZero) == 0 {
		return 1e-3 // all historical values constant → tiny floor (hypersensitive)
	}
	return Median(nonZero)
}

// =============================================================================
// Space Score Aggregation
// =============================================================================

// aggregateSpaceScores reduces per-time-point space scores to per-card
// aggregate space scores.
//
// For each card+metric:
//   spaceScore = mean of Z-Scores across detection window
//   spaceAbnormal = mean Z-Score > threshold
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

			// For absolute/direct methods, consider "abnormal" if any point had sentinel value.
			meta := MetricMetaRegistry[metric]
			isSentinel := meta.SpaceMethod == MethodAbsolute || meta.SpaceMethod == MethodDirect
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
			var spaceRef float64
			var spaceScale float64
			switch {
			case isSentinel:
				// For absolute/direct: abnormal if >50% of points flagged.
				spaceScore = float64(abnormalCount) / float64(len(zscores))
				spaceAbnormal = spaceScore > 0.5
			case isCluster:
				// Cluster method: abnormal when the card's mean z (deviation
				// energy = persistence × magnitude) exceeds the significance k.
				spaceScore = sum / float64(len(zscores))
				spaceAbnormal = spaceScore > cfg.SpaceClusterK
				// SpaceRef = window mean of the per-time-point baseline-cluster
				// mean (the peer-majority reference the card was compared to).
				refs := space.ClusterRef[cid][metric]
				var refSum float64
				for _, r := range refs {
					refSum += r
				}
				if len(refs) > 0 {
					spaceRef = refSum / float64(len(refs))
				}
				spaceScale = space.ScaleRef[cid][metric]
			default:
				spaceScore = sum / float64(len(zscores))
				spaceAbnormal = spaceScore > cfg.SpaceZThreshold
			}

			result[cid][metric] = &MetricAnomalyDetail{
				Metric:        metric,
				SpaceScore:    spaceScore,
				SpaceAbnormal: spaceAbnormal,
				SpaceRef:      spaceRef,
				SpaceScale:    spaceScale,
			}
		}
	}

	return result
}
