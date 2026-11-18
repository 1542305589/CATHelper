package detector

import (
	"sort"
	"strconv"
	"strings"

	"github.com/Computing-Availability-Tools/CATHelper/feature/straggler/config"
)

// ppWaitGapColumn is the CSV column holding the per-rank PP wait gap.
const ppWaitGapColumn = "PP_WaitGap"

// ppWaitRatioThreshold is Python's 5% PP wait-share threshold.
const ppWaitRatioThreshold = 0.05

// ppCompareGroups mirrors Python's _pp_compare_groups: use the first named
// domain among TP > CP > DP (groups with >= 2 ranks); if none exists, transpose
// the PP stage groups into per-stage compare groups.
func ppCompareGroups(parallels map[string][][]int) [][]int {
	for _, domain := range []string{"tp", "cp", "dp"} {
		groups := parallels[domain]
		if len(groups) == 0 {
			continue
		}
		var out [][]int
		for _, g := range groups {
			if len(g) < 2 {
				continue
			}
			cp := append([]int(nil), g...)
			sort.Ints(cp)
			out = append(out, cp)
		}
		if len(out) > 0 {
			return out
		}
	}

	pp := parallels[ppParallelDomainName]
	if len(pp) == 0 {
		return nil
	}
	maxLen := 0
	for _, g := range pp {
		if len(g) > maxLen {
			maxLen = len(g)
		}
	}
	var out [][]int
	for k := 0; k < maxLen; k++ {
		var members []int
		for _, g := range pp {
			if len(g) > k {
				sg := append([]int(nil), g...)
				sort.Ints(sg)
				members = append(members, sg[k])
			}
		}
		if len(members) >= 2 {
			sort.Ints(members)
			out = append(out, members)
		}
	}
	return out
}

// ppGroupKey returns the comma-separated sorted rank key.
func ppGroupKey(ranks []int) string {
	cp := append([]int(nil), ranks...)
	sort.Ints(cp)
	parts := make([]string, len(cp))
	for i, r := range cp {
		parts[i] = strconv.Itoa(r)
	}
	return strings.Join(parts, ",")
}

// DetectPPSlowDomain detects PP pipeline stragglers: within each compare group,
// a rank whose PP wait is more than 5% of the session below the group maximum is
// anomalous; the anomaly is reported once per PP communication group (max share).
func DetectPPSlowDomain(
	parallels map[string][][]int,
	stepData map[string]map[int]float64,
	localResult config.DegradationData,
	sessionNs int64,
) {
	vals := stepData[ppWaitGapColumn]
	if len(vals) == 0 || sessionNs <= 0 {
		return
	}
	compareGroups := ppCompareGroups(parallels)
	if len(compareGroups) == 0 {
		return
	}

	rankToPP := make(map[int][]int)
	for _, g := range parallels[ppParallelDomainName] {
		sg := append([]int(nil), g...)
		sort.Ints(sg)
		for _, r := range sg {
			rankToPP[r] = sg
		}
	}

	groupDeg := make(map[string]float64)
	groupRanks := make(map[string][]int)

	for _, g := range compareGroups {
		var ranks []int
		var values []float64
		for _, r := range g {
			if v, ok := vals[r]; ok && v != -99999 {
				ranks = append(ranks, r)
				values = append(values, v)
			}
		}
		if len(ranks) < 2 {
			continue
		}
		maxV := values[0]
		for _, v := range values {
			if v > maxV {
				maxV = v
			}
		}
		for i, r := range ranks {
			score := (maxV - values[i]) / float64(sessionNs)
			if score <= ppWaitRatioThreshold {
				continue
			}
			ppg := rankToPP[r]
			if len(ppg) == 0 {
				continue
			}
			key := ppGroupKey(ppg)
			if score > groupDeg[key] {
				groupDeg[key] = score
				groupRanks[key] = ppg
			}
		}
	}

	for key, deg := range groupDeg {
		localResult.AddGroup("pp_comm", groupRanks[key], deg)
	}
}
