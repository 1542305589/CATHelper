package detector

import (
	"math"
	"sort"
	"strconv"
	"strings"

	"github.com/Computing-Availability-Tools/CATHelper/feature/straggler/clustering"
	"github.com/Computing-Availability-Tools/CATHelper/feature/straggler/config"
)

// DebugRankScores computes, for every valid rank, its per-category diagnostic
// score so that --debug-output can show NORMAL ranks' values alongside flagged
// ones:
//   - cal:        top-level kmeans ratio of ZP_Kernel (max) or, when ZP_Kernel is
//     missing, ZP_Duration (min) — a normal rank sits near 1.0
//   - cpu:        top-level kmeans ratio of ZP_Host (max)
//   - npu_bubble: raw ZP_Bubble value
//
// Ranks absent from a metric simply lack that key (no data to diagnose).
func DebugRankScores(stepData map[string]map[int]float64, validRanks []int) map[int]map[string]float64 {
	scores := make(map[int]map[string]float64, len(validRanks))
	for _, r := range validRanks {
		scores[r] = map[string]float64{}
	}
	set := func(r int, cat string, v float64) {
		if s, ok := scores[r]; ok {
			s[cat] = v
		}
	}

	if kernel, ok := stepData[zpKernelColumn]; ok {
		for r, ratio := range rankRatios(kernel, config.CalThreshold, true) {
			set(r, "cal", ratio)
		}
	} else if dur, ok := stepData[zpDurationColumn]; ok {
		for r, ratio := range rankRatios(dur, config.CalThreshold, false) {
			set(r, "cal", ratio)
		}
	}
	if host, ok := stepData[zpHostDataColumn]; ok {
		for r, ratio := range rankRatios(host, config.CalThreshold, true) {
			set(r, "cpu", ratio)
		}
	}
	if bubble, ok := stepData[zpBubble]; ok {
		for r, v := range bubble {
			set(r, "npu_bubble", v)
		}
	}
	return scores
}

// rankRatios computes each rank's top-level kmeans ratio for one metric (sorted
// by rank for determinism). Uses the same single-level diagnostic as the debug
// view, not the recursive decision.
func rankRatios(data map[int]float64, threshold float64, highIsAnomaly bool) map[int]float64 {
	ranks := make([]int, 0, len(data))
	for r := range data {
		ranks = append(ranks, r)
	}
	sort.Ints(ranks)
	vals := make([]float64, len(ranks))
	for i, r := range ranks {
		vals[i] = data[r]
	}
	entries := clustering.Diagnose(vals, threshold, highIsAnomaly)
	out := make(map[int]float64, len(entries))
	for _, e := range entries {
		out[ranks[e.Index]] = e.Ratio
	}
	return out
}

// DebugCommScores returns, for every non-PP/embd communication domain, each
// group's top-level kmeans ratio (the representative = minimum-time card),
// flagged and normal groups alike, for --debug-output. Key = sorted
// comma-joined ranks ("0,1,2"), matching the anomalous comm_domain_result keys.
func DebugCommScores(stepData map[string]map[int]float64, parallels map[string][][]int) map[string]map[string]float64 {
	result := make(map[string]map[string]float64)
	for domain, groups := range parallels {
		if domain == ppParallelDomainName || domain == "embd" {
			continue
		}
		colName := domain + "_Duration"
		data, ok := stepData[colName]
		if !ok {
			continue
		}

		// Minimum-time card per group as the representative.
		var reps []int
		groupOf := make(map[int][]int)
		for _, group := range groups {
			minCard, minVal := -1, math.MaxFloat64
			for _, card := range group {
				if v, ok := data[card]; ok && v < minVal {
					minVal, minCard = v, card
				}
			}
			if minCard >= 0 {
				reps = append(reps, minCard)
				groupOf[minCard] = group
			}
		}
		if len(reps) == 0 {
			continue
		}

		vals := make([]float64, len(reps))
		for i, card := range reps {
			vals[i] = data[card]
		}
		entries := clustering.Diagnose(vals, config.CommThreshold, true)
		domainRes := make(map[string]float64)
		for _, e := range entries {
			if group, ok := groupOf[reps[e.Index]]; ok {
				domainRes[joinSortedInts(group)] = e.Ratio
			}
		}
		if len(domainRes) > 0 {
			result[domain] = domainRes
		}
	}
	return result
}

// joinSortedInts formats a rank group as the groupKey "a,b,c" (sorted).
func joinSortedInts(ranks []int) string {
	sorted := make([]int, len(ranks))
	copy(sorted, ranks)
	sort.Ints(sorted)
	parts := make([]string, len(sorted))
	for i, r := range sorted {
		parts[i] = strconv.Itoa(r)
	}
	return strings.Join(parts, ",")
}
