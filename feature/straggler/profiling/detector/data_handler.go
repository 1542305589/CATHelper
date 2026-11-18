package detector

import (
	"fmt"
	"sort"
	"strconv"

	"github.com/Computing-Availability-Tools/CATHelper/feature/straggler/config"
)

// ---------------------------------------------------------------------------
// Slow compute / metric detection
// ---------------------------------------------------------------------------

// getSlowCalculateRanks detects compute-straggler ranks by testing each group
// of the primary detection domain using KERNEL_AICORE.
func getSlowCalculateRanks(detectionGroups [][]int, alignedData map[string]map[int]float64, detectionParallel string, localResult config.DegradationData) error {
	getSlowMetricRanks(detectionGroups, alignedData, zpKernelColumn, "cal", config.CalThreshold, localResult)
	return nil
}

// getSlowMetricRanks flags one metric column across every detection group.
func getSlowMetricRanks(detectionGroups [][]int, alignedData map[string]map[int]float64, column, category string, threshold float64, localResult config.DegradationData) {
	for _, npuGroup := range detectionGroups {
		ranks, degradations := detMetricForOneGroup(alignedData, npuGroup, column, threshold)
		for i, rank := range ranks {
			localResult.AddSingle(category, rank, degradations[i])
		}
	}
}

// detMetricForOneGroup runs homogeneous clustering (max direction) on a single
// metric across one group. Invalid ranks (missing / <= 0, including -99999) are
// dropped individually; the remaining ranks are still compared (Python rule).
func detMetricForOneGroup(alignedData map[string]map[int]float64, npuGroup []int, column string, threshold float64) ([]int, []float64) {
	if len(npuGroup) < minRanksInGroup {
		return nil, nil
	}
	metricMap, ok := alignedData[column]
	if !ok {
		return nil, nil
	}
	ranks := make([]int, 0, len(npuGroup))
	values := make([]float64, 0, len(npuGroup))
	for _, npuID := range npuGroup {
		v, ok := metricMap[npuID]
		if !ok || v <= 0 {
			continue
		}
		ranks = append(ranks, npuID)
		values = append(values, v)
	}
	if len(ranks) < minRanksInGroup {
		return nil, nil
	}
	return HomogenizationComparisonFunc(ranks, values, threshold, "max")
}

// ---------------------------------------------------------------------------
// Slow CPU detection
// ---------------------------------------------------------------------------

// getSlowHostRanksByHomogenize detects CPU stragglers using ZP_Host preprocessed
// with host-based trimmed means (cards on the same physical node are grouped by
// hostUid). Anomalies are reported once per node, keyed by the node display
// name (hostName), matching Python.
func getSlowHostRanksByHomogenize(npus []int, detectionData map[int]float64, localResult config.DegradationData, rankToHostUid, rankToNodeName map[int]string) []int {
	var haveDataRanks []int
	var ranksData []float64
	for _, npuID := range npus {
		if v, ok := detectionData[npuID]; ok && v > 0 {
			haveDataRanks = append(haveDataRanks, npuID)
			ranksData = append(ranksData, v)
		}
	}
	if len(haveDataRanks) < minRanksInGroup {
		return nil
	}

	if len(rankToHostUid) > 0 {
		smoothByHostUid(ranksData, haveDataRanks, rankToHostUid)
	} else {
		// No host map: Python falls back to groups of 4 consecutive cards.
		names := smoothByChunksOfFour(ranksData, haveDataRanks)
		if rankToNodeName == nil {
			rankToNodeName = make(map[int]string, len(haveDataRanks))
		}
		for i, r := range haveDataRanks {
			if rankToNodeName[r] == "" {
				rankToNodeName[r] = names[i]
			}
		}
	}

	abnormalRanks, degradations := HomogenizationComparisonFunc(haveDataRanks, ranksData, config.CPUThreshold, "max")

	// Same-node cards share one value, so collapse to one entry per node.
	perNode := make(map[string]float64)
	for i, rank := range abnormalRanks {
		node := rankToNodeName[rank]
		if node == "" {
			node = "rank" + strconv.Itoa(rank)
		}
		if d, ok := perNode[node]; !ok || degradations[i] > d {
			perNode[node] = degradations[i]
		}
	}
	for node, d := range perNode {
		localResult.AddKey("cpu", node, d)
	}
	return abnormalRanks
}

// smoothByHostUid groups cards by their hostUid (same physical machine) and
// replaces each card's ZP_Host value with the trimmed mean of its host group.
// Cards without a hostUid mapping keep their original values unchanged.
func smoothByHostUid(ranksData []float64, ranks []int, rankToHostUid map[int]string) {
	hostGroups := make(map[string][]int)
	for i, rank := range ranks {
		uid, ok := rankToHostUid[rank]
		if !ok || uid == "" {
			continue
		}
		hostGroups[uid] = append(hostGroups[uid], i)
	}
	for _, indices := range hostGroups {
		if len(indices) <= 1 {
			continue
		}
		mean := trimmedMean(indices, ranksData)
		for _, idx := range indices {
			ranksData[idx] = mean
		}
	}
}

// smoothByChunksOfFour smooths cards in groups of 4 consecutive ranks and
// returns the synthetic node name for each index ("rank<first>-<last>").
func smoothByChunksOfFour(ranksData []float64, ranks []int) []string {
	names := make([]string, len(ranks))
	for start := 0; start < len(ranks); start += 4 {
		end := start + 4
		if end > len(ranks) {
			end = len(ranks)
		}
		idx := make([]int, 0, end-start)
		for i := start; i < end; i++ {
			idx = append(idx, i)
		}
		if len(idx) > 1 {
			mean := trimmedMean(idx, ranksData)
			for _, i := range idx {
				ranksData[i] = mean
			}
		}
		name := fmt.Sprintf("rank%d-%d", ranks[start], ranks[end-1])
		for i := start; i < end; i++ {
			names[i] = name
		}
	}
	return names
}

// trimmedMean returns the mean of the given indices, discarding the min and max
// value when there are more than two.
func trimmedMean(indices []int, vals []float64) float64 {
	if len(indices) == 0 {
		return 0
	}
	if len(indices) <= 2 {
		var sum float64
		for _, i := range indices {
			sum += vals[i]
		}
		return sum / float64(len(indices))
	}
	sorted := make([]float64, len(indices))
	for i, idx := range indices {
		sorted[i] = vals[idx]
	}
	sort.Float64s(sorted)
	trimmed := sorted[1 : len(sorted)-1]
	var sum float64
	for _, v := range trimmed {
		sum += v
	}
	return sum / float64(len(trimmed))
}
