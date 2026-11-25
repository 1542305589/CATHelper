package clustering

import (
	"math"
	"sort"
)

// MajorBandResult is one value outside every major-baseline band.
type MajorBandResult struct {
	Index  int // index into the caller's original values
	Value  float64
	Center float64 // nearest baseline cluster center (by ratio distance)
	Ratio  float64 // Value / Center
}

// DetectMajorBand implements the KPI multi-baseline space detector:
//
//  1. Values <= 0 are excluded from clustering (they cannot be a baseline and
//     must not produce a ratio-0 anomaly). Fewer than 3 input values or fewer
//     than 2 positive values -> no detection.
//  2. Z-score standardize, then elbow-method K in [2, min(n,10)] (largest second
//     difference of inertia), kmeans++ init (seed 42) and Lloyd (<=300 rounds).
//  3. Every cluster with the maximum membership is a baseline; tied maxima all
//     count.
//  4. A value is normal when it falls inside [0.7, 1.3] x any baseline center
//     (closed interval). Otherwise it is anomalous, with Ratio = Value / nearest
//     baseline center (nearest measured by |Value/Center - 1|) and Center =
//     Value / Ratio.
//
// It runs one clustering level only (no recursion). It is additive: the shared
// recursive Detect/Diagnose path used by the profiler is unchanged.
func DetectMajorBand(values []float64) ([]float64, []MajorBandResult) {
	if len(values) < 3 {
		return nil, nil
	}
	idx, vals := filterPositive(values)
	if len(vals) < 2 {
		return nil, nil
	}

	z := zscore(vals)
	k := elbowK(z)
	clusters := kmeans(z, k)

	maxSize := 0
	centers := make([]float64, len(clusters))
	sizes := make([]int, len(clusters))
	for i, cl := range clusters {
		centers[i] = clusterMean(cl, vals)
		sizes[i] = len(cl)
		if len(cl) > maxSize {
			maxSize = len(cl)
		}
	}
	var baselines []float64
	for i := range clusters {
		if sizes[i] > 0 && sizes[i] == maxSize {
			baselines = append(baselines, centers[i])
		}
	}
	sort.Float64s(baselines)

	var out []MajorBandResult
	for i, value := range vals {
		inside := false
		for _, center := range baselines {
			if center <= 0 {
				continue
			}
			if value >= center*0.7 && value <= center*1.3 {
				inside = true
				break
			}
		}
		if inside {
			continue
		}
		center, ratio := nearestBaseline(value, baselines)
		if center <= 0 {
			continue
		}
		out = append(out, MajorBandResult{
			Index:  idx[i],
			Value:  value,
			Center: center,
			Ratio:  ratio,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Index < out[j].Index })
	return baselines, out
}

// nearestBaseline selects the baseline center with the smallest ratio distance
// |value/center - 1| (not absolute |value-center|) and returns value/center.
func nearestBaseline(value float64, baselines []float64) (center, ratio float64) {
	best := 0.0
	bestDist := math.Inf(1)
	for _, c := range baselines {
		if c <= 0 {
			continue
		}
		if dist := math.Abs(value/c - 1); dist < bestDist {
			bestDist = dist
			best = c
		}
	}
	if best <= 0 {
		return 0, 0
	}
	return best, value / best
}
