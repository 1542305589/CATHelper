package detector

import (
	"strconv"
	"strings"

	"github.com/Computing-Availability-Tools/CATHelper/feature/straggler/config"
)

// ---------------------------------------------------------------------------
// Slow-domain detection by bandwidth comparison
//
// This replaces the old Duration-clustering communication detection. The
// dataparse backfill pass writes per-(opType,count) bandwidths into the CSV as
// dynamic columns "<domain>_<opType>_<count>". Here we compare, for the same
// parallel domain, the bandwidths of its rank groups pairwise: two groups'
// (opType,count) combos are comparable when their counts are within ±1.3x, and
// a group whose bandwidth is degraded (max/min >= SlowCommRatio) on more combos
// is the slow communication domain.
// ---------------------------------------------------------------------------

// slowCommMatchTolerance is the ±1.3x count tolerance within which two groups'
// (opType,count) bandwidths are considered comparable (per the skill's 口径).
const slowCommMatchTolerance = 1.3

// bwEntry is one group's (opType × count) bandwidth.
type bwEntry struct {
	opType string
	count  int
	bw     float64
}

// DetectSlowDomainByBandwidth flags slow communication groups for every
// collective parallel domain by comparing group bandwidths pairwise. Slow
// groups are written into the "comm" category (reusing comm_domain_result).
func DetectSlowDomainByBandwidth(parallels map[string][][]int, stepData map[string]map[int]float64, localResult config.DegradationData) {
	ratio := config.SlowCommRatio
	if ratio <= 0 {
		ratio = 1.3
	}

	for domain, groups := range parallels {
		if domain == ppParallelDomainName || domain == "embd" {
			continue
		}

		groupBWs := make([][]bwEntry, 0, len(groups))
		for _, group := range groups {
			groupBWs = append(groupBWs, bwSetForGroup(domain, group, stepData))
		}
		if len(groupBWs) < 2 {
			continue
		}

		// Pairwise comparison; each pair may flag the slower side.
		for a := 0; a < len(groupBWs); a++ {
			for b := a + 1; b < len(groupBWs); b++ {
				degradedA, degradedB, ratioA, ratioB := compareBWGroups(groupBWs[a], groupBWs[b], ratio)
				switch {
				case degradedA == 0 && degradedB == 0:
					// No significant degradation in any comparable combo.
				case degradedA > degradedB:
					localResult.AddGroup("comm", groups[a], ratioA)
				case degradedB > degradedA:
					localResult.AddGroup("comm", groups[b], ratioB)
				}
			}
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

// compareBWGroups compares the bandwidth sets of two groups. For each combo of
// group a, it finds the best count-matched combo of group b (same opType,
// count ratio <= slowCommMatchTolerance). Combos whose bandwidth differs by
// >= ratio are counted as degradations for the slower side. It returns the
// degradation counts for each side and the largest degradation ratio observed
// on each side.
func compareBWGroups(a, b []bwEntry, ratio float64) (degradedA, degradedB int, ratioA, ratioB float64) {
	ratioA, ratioB = 1.0, 1.0
	for _, ea := range a {
		var bestB *bwEntry
		bestDiff := -1.0
		for i := range b {
			eb := &b[i]
			if eb.opType != ea.opType {
				continue
			}
			mn, mx := ea.count, eb.count
			if eb.count < ea.count {
				mn, mx = eb.count, ea.count
			}
			if mn <= 0 {
				continue
			}
			diff := float64(mx) / float64(mn)
			if diff <= slowCommMatchTolerance && (bestDiff < 0 || diff < bestDiff) {
				bestDiff, bestB = diff, eb
			}
		}
		if bestB == nil {
			continue
		}
		bwA, bwB := ea.bw, bestB.bw
		if bwA <= 0 || bwB <= 0 {
			continue
		}
		mn, mx := bwA, bwB
		if bwB < bwA {
			mn, mx = bwB, bwA
		}
		if mx/mn >= ratio {
			switch {
			case bwA < bwB:
				degradedA++
				if mx/mn > ratioA {
					ratioA = mx / mn
				}
			case bwB < bwA:
				degradedB++
				if mx/mn > ratioB {
					ratioB = mx / mn
				}
			}
		}
	}
	return degradedA, degradedB, ratioA, ratioB
}
