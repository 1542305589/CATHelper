// Package clustering implements a unified kmeans-based 1-D ratio detection
// algorithm. It is the single anomaly-detection primitive shared by the KPI
// resource space detector (resource/space_detector.go) and the Profiler
// homogenization detector (profiling/detector/clustering.go).
//
// The semantics mirror the Python reference skill (kmeans_detector.py):
//
//  1. Filter values that are not > 0 (this also drops INVALID_MARKER=-99999).
//  2. Z-score standardize (population std; std < 1e-9 → 1).
//  3. Elbow method over k = 2..min(n,10): score(k) = 2*I[k] - I[k-1] - I[k+1]
//     for k = 3..upper-1, take the maximum (ties → smallest k); fallback 2.
//  4. kmeans++ initialization (first centroid = data[0], D² weighted sampling,
//     deterministic stream shared across the whole recursion).
//  5. Lloyd iteration (≤ 300 rounds, empty cluster takes the point farthest
//     from that cluster's current center, converge 1e-9).
//  6. Baseline = min cluster mean (max direction) / max cluster mean (min
//     direction), over NON-EMPTY clusters only. Anomalous clusters are taken
//     from the mean-sorted order and stop at the first non-anomalous cluster.
//  7. No anomalous cluster → exit.
//  8. Recurse on the UNION of anomalous clusters (depth ≤ 10); deeper results
//     replace the parent, deeper silence keeps the parent.
//  9. degradation = value / firstBaseline (max direction) or
//     firstBaseline / value (min direction), using the FIRST level's baseline.
package clustering

import (
	"math"
	"math/rand"
	"sort"
)

// maxDepth bounds the recursive cluster refinement.
const maxDepth = 10

// maxIter bounds the Lloyd iteration count.
const maxIter = 300

// convergenceEps is the centroid movement / z-score epsilon.
const convergenceEps = 1e-9

// kmeansSeed fixes the D²-sampling stream so results are reproducible.
const kmeansSeed int64 = 42

// Result is one detected anomalous data point.
type Result struct {
	Index int     // index into the original input values
	Ratio float64 // degradation: value/baseline (max) or baseline/value (min)
}

// Detect finds anomalous points in values via recursive kmeans ratio detection.
// highIsAnomaly selects the direction: true means larger is worse (baseline =
// min-mean cluster), false means smaller is worse (baseline = max-mean cluster).
// Values ≤ 0 are ignored. Every flagged point's Ratio uses the FIRST level's
// baseline: value/baseline (max) or baseline/value (min).
func Detect(values []float64, ratioThreshold float64, highIsAnomaly bool) []Result {
	if len(values) < 2 {
		return nil
	}
	rng := rand.New(rand.NewSource(kmeansSeed))
	ids := make([]int, len(values))
	for i := range ids {
		ids[i] = i
	}
	firstBase, anomalyIDs := recurseAnomaly(values, ids, 0, ratioThreshold, highIsAnomaly, rng)
	if len(anomalyIDs) == 0 {
		return nil
	}
	denom := 1.0
	if firstBase > 0 {
		denom = firstBase
	}
	out := make([]Result, 0, len(anomalyIDs))
	for _, i := range anomalyIDs {
		if i < 0 || i >= len(values) {
			continue
		}
		ratio := 1.0
		switch {
		case highIsAnomaly:
			ratio = values[i] / denom
		case values[i] > 0:
			ratio = denom / values[i]
		}
		out = append(out, Result{Index: i, Ratio: ratio})
	}
	return out
}

// DiagnoseEntry is one data point's diagnostic at a single kmeans level.
type DiagnoseEntry struct {
	Index   int     // index into the original input values
	Value   float64 // original value
	Cluster int     // cluster id at this level (-1 when the value was filtered out)
	Ratio   float64 // cluster mean / baseline mean (0 when not on the anomaly side)
	Flagged bool    // ratio > threshold (max) or ratio < 1/threshold (min)
}

// Diagnose runs one top-level kmeans level on values and reports EVERY point's
// cluster, ratio and flag — the debug counterpart of Detect. It does not
// recurse.
func Diagnose(values []float64, ratioThreshold float64, highIsAnomaly bool) []DiagnoseEntry {
	entries := make([]DiagnoseEntry, len(values))
	for i, v := range values {
		entries[i] = DiagnoseEntry{Index: i, Value: v, Cluster: -1}
	}
	idx, vals := filterPositive(values)
	if len(vals) < 2 {
		return entries
	}

	rng := rand.New(rand.NewSource(kmeansSeed))
	z := zscore(vals)
	k := elbowK(z, rng)
	clusters := kmeans(z, k, rng)
	baseIdx := pickBaselineCluster(clusters, vals, highIsAnomaly)
	if baseIdx < 0 {
		return entries
	}
	baseMean := clusterMean(clusters[baseIdx], vals)
	if baseMean <= 0 {
		baseMean = math.SmallestNonzeroFloat64
	}

	clusterOf := make(map[int]int, len(vals))
	clusterMeans := make([]float64, len(clusters))
	for cid, cl := range clusters {
		clusterMeans[cid] = clusterMean(cl, vals)
		for _, li := range cl {
			clusterOf[idx[li]] = cid
		}
	}

	for i := range entries {
		cid, ok := clusterOf[i]
		if !ok {
			continue
		}
		entries[i].Cluster = cid
		ratio := clusterMeans[cid] / baseMean
		entries[i].Ratio = ratio
		if highIsAnomaly {
			entries[i].Flagged = ratio > ratioThreshold
		} else {
			entries[i].Flagged = ratio < 1.0/ratioThreshold
		}
	}
	return entries
}

// recurseAnomaly mirrors Python's _recurse_anomaly: filter, detect one level,
// then recurse on the union of anomalous points. A deeper anomaly replaces the
// parent; deeper silence keeps the parent.
func recurseAnomaly(data []float64, ids []int, depth int, threshold float64, highIsAnomaly bool, rng *rand.Rand) (float64, []int) {
	var vd []float64
	var vi []int
	for i, v := range data {
		if v > 0 {
			vd = append(vd, v)
			vi = append(vi, ids[i])
		}
	}
	if len(vd) < 2 {
		return 0, nil
	}
	anomalyVals, anomalyIDs, baseline := kmeansAnomalyDetect(vd, vi, threshold, highIsAnomaly, rng)
	if len(anomalyVals) == 0 {
		return baseline, nil
	}
	if depth >= maxDepth {
		return baseline, anomalyIDs
	}
	_, subIDs := recurseAnomaly(anomalyVals, anomalyIDs, depth+1, threshold, highIsAnomaly, rng)
	if len(subIDs) > 0 {
		return baseline, subIDs
	}
	return baseline, anomalyIDs
}

// kmeansAnomalyDetect mirrors Python's _kmeans_anomaly_detect.
func kmeansAnomalyDetect(data []float64, ids []int, threshold float64, highIsAnomaly bool, rng *rand.Rand) ([]float64, []int, float64) {
	n := len(data)
	if n < 2 {
		return nil, nil, 0
	}
	z := zscore(data)
	k := elbowK(z, rng)
	clusters := kmeans(z, k, rng)

	means := make(map[int]float64, len(clusters))
	for c, cl := range clusters {
		if len(cl) > 0 {
			means[c] = clusterMean(cl, data)
		}
	}
	if len(means) == 0 {
		return nil, nil, 0
	}

	baseline := math.Inf(1)
	if !highIsAnomaly {
		baseline = math.Inf(-1)
	}
	for _, m := range means {
		if highIsAnomaly && m < baseline {
			baseline = m
		}
		if !highIsAnomaly && m > baseline {
			baseline = m
		}
	}

	order := make([]int, 0, len(means))
	for c := range means {
		order = append(order, c)
	}
	sort.Slice(order, func(i, j int) bool {
		if highIsAnomaly {
			return means[order[i]] > means[order[j]]
		}
		return means[order[i]] < means[order[j]]
	})

	isAnomaly := make(map[int]bool, len(order))
	for _, c := range order {
		m := means[c]
		if highIsAnomaly {
			if m > baseline*threshold {
				isAnomaly[c] = true
				continue
			}
			break
		}
		if m < baseline && m/baseline < 1.0/threshold {
			isAnomaly[c] = true
			continue
		}
		break
	}
	if len(isAnomaly) == 0 {
		return nil, nil, baseline
	}

	labelOf := make([]int, n)
	for c, cl := range clusters {
		for _, j := range cl {
			labelOf[j] = c
		}
	}
	var anomalyVals []float64
	var anomalyIDs []int
	for j := 0; j < n; j++ {
		if isAnomaly[labelOf[j]] {
			anomalyVals = append(anomalyVals, data[j])
			anomalyIDs = append(anomalyIDs, ids[j])
		}
	}
	return anomalyVals, anomalyIDs, baseline
}

// filterPositive keeps values > 0 and their original indices.
func filterPositive(values []float64) (idx []int, vals []float64) {
	for i, v := range values {
		if v > 0 {
			idx = append(idx, i)
			vals = append(vals, v)
		}
	}
	return idx, vals
}

// zscore standardizes vals to zero mean / unit std; a std below eps is forced
// to 1.
func zscore(vals []float64) []float64 {
	n := len(vals)
	if n == 0 {
		return nil
	}
	var sum float64
	for _, v := range vals {
		sum += v
	}
	mean := sum / float64(n)
	var sq float64
	for _, v := range vals {
		d := v - mean
		sq += d * d
	}
	std := math.Sqrt(sq / float64(n))
	if std < convergenceEps {
		std = 1
	}
	z := make([]float64, n)
	for i, v := range vals {
		z[i] = (v - mean) / std
	}
	return z
}

// elbowK computes inertias for k = 2..min(n,10) and applies pickElbowK.
func elbowK(z []float64, rng *rand.Rand) int {
	upper := len(z)
	if upper > 10 {
		upper = 10
	}
	if upper < 2 {
		return 2
	}
	inertias := make([]float64, upper+1)
	for k := 2; k <= upper; k++ {
		inertias[k] = kmeansInertia(z, k, rng)
	}
	return pickElbowK(inertias)
}

// pickElbowK applies Python's elbow rule: score(k) = 2*I[k] - I[k-1] - I[k+1]
// over k = 3..upper-1, maximum wins (ties → smallest k). inertias is indexed by
// k; index 0/1 are unused.
func pickElbowK(inertias []float64) int {
	upper := len(inertias) - 1
	if upper < 2 {
		return 2
	}
	bestK := 2
	bestScore := math.Inf(-1)
	found := false
	for k := 3; k <= upper-1; k++ {
		score := 2*inertias[k] - inertias[k-1] - inertias[k+1]
		if !found || score > bestScore {
			bestK, bestScore, found = k, score, true
		}
	}
	if !found {
		return 2
	}
	return bestK
}

// kmeansInertia is the within-cluster sum of squared distances for a k-cluster
// fit (used only by the elbow method).
func kmeansInertia(z []float64, k int, rng *rand.Rand) float64 {
	clusters := kmeans(z, k, rng)
	var inertia float64
	for _, cl := range clusters {
		if len(cl) == 0 {
			continue
		}
		m := clusterMean(cl, z)
		for _, i := range cl {
			d := z[i] - m
			inertia += d * d
		}
	}
	return inertia
}

// kmeans clusters z into k clusters (indices into z) via kmeans++ init and
// Lloyd iteration, mirroring Python's _kmeans. The rng is shared across the
// whole recursion (it is NOT re-seeded per call).
func kmeans(z []float64, k int, rng *rand.Rand) [][]int {
	n := len(z)
	if n == 0 {
		return nil
	}
	if k < 1 {
		k = 1
	}
	if k <= 1 || n < 2 {
		cl := make([]int, n)
		for i := range cl {
			cl[i] = i
		}
		out := make([][]int, k)
		out[0] = cl
		return out
	}
	if k > n {
		k = n
	}

	centers := kmeansPP(z, k, rng)
	k = len(centers)
	if k < 1 {
		k = 1
	}

	labels := make([]int, n)
	for iter := 0; iter < maxIter; iter++ {
		newLabels := make([]int, n)
		assignChanged := false
		for i, x := range z {
			b := nearestCentroidIndex(x, centers)
			newLabels[i] = b
			if b != labels[i] {
				assignChanged = true
			}
		}

		sums := make([]float64, k)
		cnts := make([]int, k)
		for j, x := range z {
			c := newLabels[j]
			sums[c] += x
			cnts[c]++
		}

		newCenters := make([]float64, k)
		for c := 0; c < k; c++ {
			if cnts[c] > 0 {
				newCenters[c] = sums[c] / float64(cnts[c])
				continue
			}
			farthestJ, farthestD := -1, -1.0
			for j, x := range z {
				d := x - centers[c]
				d *= d
				if d > farthestD {
					farthestD, farthestJ = d, j
				}
			}
			if farthestJ >= 0 {
				newCenters[c] = z[farthestJ]
			} else {
				newCenters[c] = centers[c]
			}
		}

		shift := 0.0
		for c := 0; c < k; c++ {
			if d := math.Abs(newCenters[c] - centers[c]); d > shift {
				shift = d
			}
		}
		centers = newCenters
		labels = newLabels
		if shift < convergenceEps && !assignChanged {
			break
		}
	}

	clusters := make([][]int, k)
	for i, l := range labels {
		if l >= 0 && l < k {
			clusters[l] = append(clusters[l], i)
		}
	}
	return clusters
}

// kmeansPP mirrors Python's _kmeans_pp_seed: first center = data[0], then
// D²-weighted sampling; a chosen index already in the set is skipped (the next
// draw is used). When all remaining distances are zero, the first unused index
// is appended. Returns the chosen centers' values.
func kmeansPP(z []float64, k int, rng *rand.Rand) []float64 {
	n := len(z)
	indices := []int{0}
	centers := []float64{z[0]}
	for len(indices) < k {
		distSq := make([]float64, n)
		total := 0.0
		for i := range z {
			minD := math.Inf(1)
			for _, c := range centers {
				d := z[i] - c
				d *= d
				if d < minD {
					minD = d
				}
			}
			distSq[i] = minD
			total += minD
		}
		if total <= 0 {
			added := false
			for i := 0; i < n; i++ {
				if !containsInt(indices, i) {
					indices = append(indices, i)
					centers = append(centers, z[i])
					added = true
					break
				}
			}
			if !added {
				break
			}
			continue
		}
		r := rng.Float64() * total
		acc := 0.0
		chosen := -1
		for i, d := range distSq {
			acc += d
			if acc >= r {
				chosen = i
				break
			}
		}
		if chosen < 0 {
			chosen = n - 1
		}
		if !containsInt(indices, chosen) {
			indices = append(indices, chosen)
			centers = append(centers, z[chosen])
		}
	}
	return centers
}

// containsInt reports whether v is in xs.
func containsInt(xs []int, v int) bool {
	for _, x := range xs {
		if x == v {
			return true
		}
	}
	return false
}

// nearestCentroid returns the value of the closest centroid to v.
func nearestCentroid(v float64, centroids []float64) float64 {
	return centroids[nearestCentroidIndex(v, centroids)]
}

// nearestCentroidIndex returns the index of the closest centroid to v.
func nearestCentroidIndex(v float64, centroids []float64) int {
	best := 0
	bestDist := math.Abs(v - centroids[0])
	for i := 1; i < len(centroids); i++ {
		if d := math.Abs(v - centroids[i]); d < bestDist {
			bestDist = d
			best = i
		}
	}
	return best
}

// pickBaselineCluster returns the direction extreme cluster: for highIsAnomaly
// (max) the smallest-mean cluster; for lowIsAnomaly (min) the largest-mean
// cluster. Empty clusters are ignored. Returns -1 when every cluster is empty.
func pickBaselineCluster(clusters [][]int, vals []float64, highIsAnomaly bool) int {
	best := -1
	var bestMean float64
	for i, cl := range clusters {
		if len(cl) == 0 {
			continue
		}
		m := clusterMean(cl, vals)
		if best < 0 || (highIsAnomaly && m < bestMean) || (!highIsAnomaly && m > bestMean) {
			best, bestMean = i, m
		}
	}
	return best
}

// clusterMean returns the mean of a cluster (list of indices into vals).
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
