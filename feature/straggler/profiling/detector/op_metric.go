package detector

import (
	"sort"
	"strconv"
)

// OpMetricRank is one rank's op_metric artifacts in their reported JSON form —
// the same shape served by the daemon's /straggler/op_metric view. group_info /
// host_info / npu_info are the parsed JSON files; global_rank is the CSV turned
// into a JSON object (single row) or array (multiple rows).
type OpMetricRank struct {
	GroupInfo  map[string]any `json:"group_info"`
	HostInfo   map[string]any `json:"host_info"`
	NpuInfo    map[string]any `json:"npu_info"`
	GlobalRank any            `json:"global_rank"`
}

// OpMetric is the aggregated per-rank op_metric data, keyed by the rank's
// decimal string ("0", "1", ...). It is the interchange format between the
// daemon (which reports it to the center) and the center (which re-runs
// detection on it, possibly merging several daemons' data).
type OpMetric map[string]*OpMetricRank

// BuildDetectionInput reconstructs the detector inputs from in-memory op_metric
// data, mirroring what GetCurDetectionInfo / GetCurJobLastStepData /
// GetHostUidMapping read from the op_metric/ files. This lets the center node
// run the full detection pipeline on reported JSON without materializing files.
func BuildDetectionInput(op OpMetric) (parallels map[string][][]int, validRanks []int, stepData map[string]map[int]float64, hostUid map[int]string) {
	// validRanks + the per-rank group_info topologies.
	rankSet := make(map[int]bool, len(op))
	var topos []map[string]interface{}
	for rankStr, r := range op {
		rank, err := strconv.Atoi(rankStr)
		if err != nil {
			continue
		}
		rankSet[rank] = true
		if r != nil && r.GroupInfo != nil {
			topos = append(topos, r.GroupInfo)
		}
	}
	validRanks = sortedKeys(rankSet)

	parallels = buildParallels(topos)
	stepData = buildStepDataFromOpMetric(op, validRanks)
	hostUid = buildHostUidFromOpMetric(op, validRanks)
	return parallels, validRanks, stepData, hostUid
}

// buildParallels collects the domain set from the per-rank group_info
// topologies and, per domain, builds the deduplicated rank groups (keep only
// groups with >1 card). This is shared between the file-backed path
// (GetCurDetectionInfo) and the in-memory path (BuildDetectionInput).
func buildParallels(topos []map[string]interface{}) map[string][][]int {
	domainSet := make(map[string]bool)
	for _, topo := range topos {
		if topo == nil {
			continue
		}
		for _, v := range topo {
			if m, ok := v.(map[string]interface{}); ok {
				if gn, ok := m[dataFileFieldGroupName].(string); ok && gn != "" {
					domainSet[gn] = true
				}
			}
		}
	}

	parallels := make(map[string][][]int)
	for domain := range domainSet {
		groups := buildParallelGroups(topos, domain)
		var filtered [][]int
		for _, g := range groups {
			if len(g) > 1 {
				filtered = append(filtered, g)
			}
		}
		if len(filtered) > 0 {
			parallels[domain] = filtered
		}
	}
	return parallels
}

// buildParallelGroups collects all rank groups for one domain across the
// per-rank group_info topologies, deduplicated (a group whose ranks are already
// assigned is skipped — each rank's topology lists the same group).
func buildParallelGroups(topos []map[string]interface{}, target string) [][]int {
	var groups [][]int
	assigned := make(map[int]bool)
	for _, topo := range topos {
		if topo == nil {
			continue
		}
		for _, v := range topo {
			m, ok := v.(map[string]interface{})
			if !ok {
				continue
			}
			gn, _ := m[dataFileFieldGroupName].(string)
			if gn != target {
				continue
			}
			ranksRaw, ok := m[dataFileFieldGlobalRanks].([]interface{})
			if !ok {
				continue
			}
			npuGroup := make([]int, 0, len(ranksRaw))
			for _, r := range ranksRaw {
				switch n := r.(type) {
				case float64:
					npuGroup = append(npuGroup, int(n))
				case int:
					npuGroup = append(npuGroup, n)
				}
			}
			if len(npuGroup) == 0 {
				continue
			}
			if rankAlreadyAssigned(assigned, npuGroup) {
				continue
			}
			for _, rank := range npuGroup {
				assigned[rank] = true
			}
			sort.Ints(npuGroup)
			groups = append(groups, npuGroup)
		}
	}
	return groups
}

// buildStepDataFromOpMetric turns each rank's global_rank JSON into the unified
// snapshot map (metric → rank → value), skipping the StepIndex column and the
// -99999 sentinel. A single-row object is used as-is; a multi-row array uses the
// second-to-last row (mirroring GetCurJobLastStepData).
func buildStepDataFromOpMetric(op OpMetric, validRanks []int) map[string]map[int]float64 {
	result := make(map[string]map[int]float64)
	for _, rank := range validRanks {
		r, ok := op[strconv.Itoa(rank)]
		if !ok || r == nil || r.GlobalRank == nil {
			continue
		}
		row := globalRankRow(r.GlobalRank)
		for col, v := range row {
			if col == stepIndex {
				continue
			}
			fv, ok := toFloat(v)
			if !ok || fv == -99999.0 {
				continue
			}
			if result[col] == nil {
				result[col] = make(map[int]float64)
			}
			result[col][rank] = fv
		}
	}
	return result
}

// globalRankRow extracts the representative data row from a global_rank value:
// a single object is returned as-is; an array yields the second-to-last row
// (or the only row).
func globalRankRow(g any) map[string]any {
	switch v := g.(type) {
	case map[string]any:
		return v
	case []any:
		if len(v) == 0 {
			return nil
		}
		idx := len(v) - 1
		if len(v) > 1 {
			idx = len(v) - 2
		}
		if m, ok := v[idx].(map[string]any); ok {
			return m
		}
	}
	return nil
}

// toFloat converts a JSON number or numeric string to float64.
func toFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case string:
		f, err := strconv.ParseFloat(n, 64)
		return f, err == nil
	}
	return 0, false
}

// buildHostUidFromOpMetric builds rank → hostUid from the in-memory host_info
// data (mirrors GetHostUidMapping). Ranks without a hostUid are omitted.
func buildHostUidFromOpMetric(op OpMetric, validRanks []int) map[int]string {
	mapping := make(map[int]string)
	for _, rank := range validRanks {
		r, ok := op[strconv.Itoa(rank)]
		if !ok || r == nil || r.HostInfo == nil {
			continue
		}
		if uid, ok := r.HostInfo["hostUid"].(string); ok && uid != "" {
			mapping[rank] = uid
		}
	}
	return mapping
}
