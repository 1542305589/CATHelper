package detector

import (
	"strconv"
	"strings"

	"github.com/Computing-Availability-Tools/CATHelper/feature/straggler/clustering"
	"github.com/Computing-Availability-Tools/CATHelper/feature/straggler/config"
)

// ---------------------------------------------------------------------------
// Slow-domain detection by bandwidth clustering
//
// The dataparse backfill pass writes per-(opType,count) bandwidths into the CSV
// as dynamic columns "<domain>_<opType>_<count>". For each collective parallel
// domain, we take each (opType,count) combo and cluster the per-group bandwidth
// of that combo with the shared kmeans recursive detector (min direction: lower
// bandwidth is slower) using SlowCommRatio as the threshold. A group is
// reported when it is flagged on at least one combo; its reported degradation
// is the largest ratio across all its flagged combos.
// ---------------------------------------------------------------------------

// bwEntry is one group's (opType × count) bandwidth.
type bwEntry struct {
	opType string
	count  int
	bw     float64
}

// DetectSlowDomainByBandwidth flags slow communication groups for every
// collective parallel domain by clustering per-(opType,count) bandwidths with
// the shared kmeans detector. Slow groups are written into the "comm" category
// (reusing comm_domain_result).
func DetectSlowDomainByBandwidth(parallels map[string][][]int, stepData map[string]map[int]float64, localResult config.DegradationData) {
	ratio := config.SlowCommRatio
	if ratio <= 0 {
		ratio = 1.3
	}

	for domain, groups := range parallels {
		if domain == ppParallelDomainName || domain == "embd" {
			continue
		}
		if len(groups) < 2 {
			continue
		}

		// Per-group bandwidth entries (each group may miss some combos).
		groupBWs := make([][]bwEntry, len(groups))
		for i, group := range groups {
			groupBWs[i] = bwSetForGroup(domain, group, stepData)
		}

		// Collect every (opType,count) combo across the domain, together with
		// the (group index, bandwidth) of each group that has it.
		type comboKey struct {
			opType string
			count  int
		}
		type comboEntry struct {
			groupIdx int
			bw       float64
		}
		comboMap := make(map[comboKey][]comboEntry)
		for gi, bws := range groupBWs {
			for _, e := range bws {
				k := comboKey{e.opType, e.count}
				comboMap[k] = append(comboMap[k], comboEntry{groupIdx: gi, bw: e.bw})
			}
		}

		// Across ALL combos of this domain, collect every flagged group's
		// degradation and report only the single group with the largest one.
		bestGroup := -1
		bestDeg := 0.0

		for _, entries := range comboMap {
			if len(entries) < 2 {
				continue // need ≥2 groups with this combo to cluster
			}
			bws := make([]float64, len(entries))
			for i, e := range entries {
				bws[i] = e.bw
			}
			// min direction: lower bandwidth = slower = anomalous.
			for _, r := range clustering.Detect(bws, ratio, false) {
				gi := entries[r.Index].groupIdx
				deg := 1.0 / r.Ratio // baseline/value → >1, larger = slower
				if deg > bestDeg {
					bestDeg = deg
					bestGroup = gi
				}
			}
		}

		if bestGroup >= 0 {
			localResult.AddGroup("comm", groups[bestGroup], bestDeg)
		}
	}
}

// bwSetForGroup collects the bandwidth for every (opType,count) column of one
// domain group. All ranks of a group share the same backfilled bandwidth, so
// the first rank that has the value is used as the representative.
func bwSetForGroup(domain string, group []int, stepData map[string]map[int]float64) []bwEntry {
	prefix := domain + "_"
	var out []bwEntry
	for col, byRank := range stepData {
		opType, count, ok := parseBandwidthCol(prefix, col)
		if !ok {
			continue
		}
		var v float64
		found := false
		for _, r := range group {
			if val, ok := byRank[r]; ok {
				v, found = val, true
				break
			}
		}
		if !found {
			continue
		}
		out = append(out, bwEntry{opType: opType, count: count, bw: v})
	}
	return out
}

// parseBandwidthCol parses a bandwidth column name "<opType>_<count>" (given
// the domain prefix, e.g. "tp_") into its op type and count. It returns false
// for the diagnostic Duration/Count columns and any non-numeric tail.
func parseBandwidthCol(prefix, col string) (string, int, bool) {
	if !strings.HasPrefix(col, prefix) {
		return "", 0, false
	}
	rest := strings.TrimPrefix(col, prefix)
	rest = strings.TrimPrefix(rest, "_")
	idx := strings.LastIndex(rest, "_")
	if idx <= 0 || idx == len(rest)-1 {
		return "", 0, false
	}
	opType := rest[:idx]
	countStr := rest[idx+1:]
	count, err := strconv.Atoi(countStr)
	if err != nil {
		return "", 0, false
	}
	if opType == "Duration" || opType == "Count" {
		return "", 0, false
	}
	return opType, count, true
}
