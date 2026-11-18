package detector

import (
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

	// 1. Select primary detection group.
	calGroupName, calGroups := GetCalDetectionGroup(parallels, validRanks)
	if len(calGroupName) == 0 || len(calGroups) == 0 {
		return nil
	}

	// 2. Slow compute detection (KERNEL_AICORE + kernel_aivec + memcpy_async).
	_ = getSlowCalculateRanks(calGroups, stepData, calGroupName, localResult)
	getSlowMetricRanks(calGroups, stepData, kernelAivecColumn, "kernel_aivec", config.CalThreshold, localResult)
	getSlowMetricRanks(calGroups, stepData, memcpyAsyncColumn, "memcpy_async", config.CPUThreshold, localResult)

	// 3. Slow communication detection (bandwidth comparison per domain group).
	DetectSlowDomainByBandwidth(parallels, stepData, localResult)

	// 3b. PP slow communication: skipped when a compute anomaly was found.
	if len(localResult["cal"]) == 0 && len(localResult["kernel_aivec"]) == 0 {
		DetectPPSlowDomain(parallels, stepData, localResult, config.PPSessionNs)
	}

	// 4. Slow CPU detection (node-level; anomalous cards collapse to one entry).
	if hostData, ok := stepData[zpHostDataColumn]; ok {
		hostUidMapping := GetHostUidMapping(config.FilePath, validRanks)
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
func GetCalDetectionGroup(parallels map[string][][]int, curNpus []int) (string, [][]int) {
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
	// No recognized parallel domain (e.g. group names not registered in the
	// profiler data, or only unknown domains like "mc2" present): degrade to
	// a single group of all ranks so cal-only detection can still run. Slow
	// comm/CPU have no data without group names and stay silent.
	if len(curNpus) >= minRanksInGroup {
		return defaultGroupParallelDomainName, [][]int{curNpus}
	}
	return "", nil
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
