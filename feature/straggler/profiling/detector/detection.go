package detector

import (
	"sort"

	"github.com/Computing-Availability-Tools/CATHelper/feature/straggler/config"
)

// ---------------------------------------------------------------------------
// DelimitDetection — main detection pipeline
// ---------------------------------------------------------------------------

// DelimitDetection orchestrates all four detection categories and returns
// the aggregated results as a DegradationData map.
//
// Pipeline:
//  1. Select primary detection group.
//  2. Detect slow compute (homogeneous clustering on primary group).
//  3. Detect slow communication (per-domain group bandwidth comparison).
//  4. Detect PP slow communication (PP wait share).
//  5. Detect slow CPU (homogeneous clustering with hostUid-based trim preprocessing).
func DelimitDetection(
	stepData map[string]map[int]float64,
	parallels map[string][][]int,
	validRanks []int,
) config.DegradationData {
	// Parallels may be empty when the profiler data has no registered group
	// names; GetCalDetectionGroup degrades to a single all-ranks group so
	// cal-only detection can still run.
	if len(stepData) == 0 || len(validRanks) == 0 {
		return nil
	}

	localResult := config.NewDegradationData()

	// Host mapping is used by CPU detection and by the no-named-domain fallback.
	hostUidMapping := GetHostUidMapping(config.FilePath, validRanks)

	// 1. Select primary detection group.
	calGroupName, calGroups := GetCalDetectionGroup(parallels, validRanks, hostUidMapping)
	if len(calGroups) == 0 {
		return nil
	}

	// 2. Slow compute detection (KERNEL_AICORE + kernel_aivec + memcpy_async).
	_ = getSlowCalculateRanks(calGroups, stepData, calGroupName, localResult)
	getSlowMetricRanks(calGroups, stepData, kernelAivecColumn, "kernel_aivec", config.CalThreshold, localResult)
	getSlowMetricRanks(calGroups, stepData, memcpyAsyncColumn, "memcpy_async", config.CPUThreshold, localResult)

	// 3. Slow communication detection (bandwidth comparison per domain group).
	// Only meaningful when the data has named parallel domains.
	if config.HasNamedDomain {
		DetectSlowDomainByBandwidth(parallels, stepData, localResult)

		// 3b. PP slow communication: skipped when a compute anomaly was found.
		if len(localResult["cal"]) == 0 && len(localResult["kernel_aivec"]) == 0 {
			DetectPPSlowDomain(parallels, stepData, localResult, config.PPSessionNs)
		}
	}

	// 4. Slow CPU detection (node-level; anomalous cards collapse to one entry).
	if hostData, ok := stepData[zpHostDataColumn]; ok {
		nodeNames := GetNodeNameMapping(config.FilePath, validRanks)
		getSlowHostRanksByHomogenize(validRanks, hostData, localResult, hostUidMapping, nodeNames)
	}

	return localResult
}

// ---------------------------------------------------------------------------
// GetCalDetectionGroup — select the primary detection domain
// ---------------------------------------------------------------------------

// GetCalDetectionGroup selects the highest-priority parallel domain that is
// present in the data and returns its name and rank groups (filtered to
// only include ranks present in this node's data).
//
// Priority order: tp → exp → ep → tp_exp → cp → cp2 → cp_ulysses → cp_ring →
// dp → dp_cp → dp_modulo_exp_cp
func GetCalDetectionGroup(parallels map[string][][]int, curNpus []int, hostUid map[int]string) (string, [][]int) {
	npuSet := make(map[int]bool, len(curNpus))
	for _, n := range curNpus {
		npuSet[n] = true
	}

	for _, domain := range detectionPriority {
		groups, ok := parallels[domain]
		if !ok || len(groups) == 0 {
			continue
		}
		filtered := getDetectionGroups(groups, npuSet)
		if len(filtered) > 0 {
			return domain, filtered
		}
	}
	// No recognized parallel domain: fall back to physical-node groups by hostUid
	// (Python's no-named-domain degradation).
	if groups := hostUidGroups(curNpus, hostUid); len(groups) > 0 {
		return "", groups
	}
	// Fallback when there is no host mapping either: a single all-ranks group so
	// cal-only detection can still run.
	if len(curNpus) >= minRanksInGroup {
		return defaultGroupParallelDomainName, [][]int{curNpus}
	}
	return "", nil
}

// hostUidGroups groups ranks by physical node (hostUid), keeping groups with at
// least two cards, sorted by their smallest rank.
func hostUidGroups(ranks []int, hostUid map[int]string) [][]int {
	if len(hostUid) == 0 {
		return nil
	}
	byHost := make(map[string][]int)
	for _, r := range ranks {
		if h, ok := hostUid[r]; ok && h != "" {
			byHost[h] = append(byHost[h], r)
		}
	}
	var out [][]int
	for _, g := range byHost {
		if len(g) >= minRanksInGroup {
			sort.Ints(g)
			out = append(out, g)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i][0] < out[j][0] })
	return out
}

// getDetectionGroups filters groups to only include ranks that are present in
// the node's valid rank set (cross-node topology handling).
func getDetectionGroups(groups [][]int, nodeGlobalRank map[int]bool) [][]int {
	var result [][]int
	for _, group := range groups {
		var valid []int
		for _, rank := range group {
			if nodeGlobalRank[rank] {
				valid = append(valid, rank)
			}
		}
		if len(valid) > 0 {
			result = append(result, valid)
		}
	}
	return result
}
