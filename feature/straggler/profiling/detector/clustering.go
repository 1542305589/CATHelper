// Package spacedetector implements the homogeneous clustering algorithm — a
// recursive 1-D clustering detector that finds abnormal data points by
// locating the largest gap in a sorted list of values.
//
// It is the single anomaly-detection primitive used by all four detection
// categories (slow compute, slow communication, slow CPU, NPU bubble).
package detector

import (
	"math"
	"sort"
)

// HomogenizationComparisonFunc is the public entry point.
//
// Parameters:
//   - fileRanks:           original rank IDs (e.g. [1, 5, 9, 13])
//   - alignedData:         data values, one per rank
//   - degradationPercent:  threshold (e.g. 1.3 for compute, 2.5 for communication)
//   - abnormalType:        "max" (bigger is worse) or "min" (smaller is worse)
//
// Returns:
//   - abnormal rank IDs
//   - corresponding degradation scores (value / baseline for "max", baseline / value for "min")
//
// It shares the KPI resource MethodCluster's structural philosophy — full
// two-sided decomposition to separate all anomaly levels, then the largest
// cluster is the majority baseline — but with RATIO significance, since the
// Profiler is a single snapshot with no historical baseline to provide a noise
// scale.
func HomogenizationComparisonFunc(
	fileRanks []int,
	alignedData []float64,
	degradationPercent float64,
	abnormalType string,
) ([]int, []float64) {
	if len(alignedData) == 0 || len(fileRanks) == 0 || len(alignedData) != len(fileRanks) {
		return nil, nil
	}
	if len(alignedData) < 2 {
		return nil, nil
	}

	// 1. Full decomposition: recursively split at the largest gap on BOTH
	//    sides until no sub-group has a dominant gap. Unlike the old
	//    anomaly-side-only recursion, this separates intermediate anomaly
	//    levels (e.g. a mildly-slow group) from the normal core.
	sortedIdx := make([]int, len(alignedData))
	for i := range sortedIdx {
		sortedIdx[i] = i
	}
	sort.Slice(sortedIdx, func(a, b int) bool { return alignedData[sortedIdx[a]] < alignedData[sortedIdx[b]] })
	clusters := gapSplitClusters(sortedIdx, alignedData)

	// 2. Baseline = the largest cluster (the peer majority); ties broken
	//    toward the direction extreme ("max" → lower mean, "min" → higher).
	baseIdx := pickBaselineCluster(clusters, alignedData, abnormalType)
	baseMean := clusterMean(clusters[baseIdx], alignedData)
	if baseMean <= 0 {
		baseMean = math.SmallestNonzeroFloat64
	}

	// 3. Baseline members are exempt (they ARE the normal reference); each
	//    non-baseline card on the anomaly side is flagged if its ratio to the
	//    baseline mean meets the degradation threshold.
	baseMembers := make(map[int]bool, len(clusters[baseIdx]))
	for _, k := range clusters[baseIdx] {
		baseMembers[k] = true
	}

	var abnormalRanks []int
	var degradations []float64
	for _, idx := range sortedIdx {
		if baseMembers[idx] {
			continue // majority = the reference, not judged
		}
		v := alignedData[idx]
		var ratio float64
		switch abnormalType {
		case "min":
			if v >= baseMean {
				continue // one-sided: only the anomaly direction
			}
			ratio = baseMean / v
		default: // "max"
			if v <= baseMean {
				continue
			}
			ratio = v / baseMean
		}
		if ratio >= degradationPercent {
			abnormalRanks = append(abnormalRanks, fileRanks[idx])
			degradations = append(degradations, ratio)
		}
	}
	return abnormalRanks, degradations
}

// gapSplitClusters recursively splits the sorted present values at the largest
// adjacent gap, on BOTH sides, until no sub-group has a gap ≥ half its own
// span. It returns a full partition of index lists (indices into the values
// slice). This mirrors the KPI resource MethodCluster decomposition.
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
// "max" (bigger is abnormal) → lower mean; "min" → higher mean.
func pickBaselineCluster(clusters [][]int, vals []float64, abnormalType string) int {
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
		if (abnormalType == "max" && cMean < bMean) || (abnormalType == "min" && cMean > bMean) {
			best = i
		}
	}
	return best
}
