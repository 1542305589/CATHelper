// Package clustering implements a unified kmeans-based 1-D ratio detection
// algorithm. It is the single anomaly-detection primitive shared by the KPI
// resource space detector (resource/space_detector.go) and the Profiler
// homogenization detector (profiling/detector/clustering.go).
//
// The algorithm replaces the previous "gap-split + majority-baseline" scheme
// with kmeans ratio detection:
//
//  1. Filter values ≤ 0; if fewer than 2 remain → no anomaly.
//  2. Z-score standardize (std ≈ 0 → forced to 1).
//  3. Elbow method selects k (k = 2..min(n,10), max inertia second difference).
//  4. kmeans++ initialization (first centroid = data[0], D² weighted sampling).
//  5. Lloyd iteration (≤ 300 rounds, empty-cluster handling, converge 1e-9).
//  6. Baseline cluster = direction extreme cluster (max → min-mean cluster;
//     min → max-mean cluster). Score = cluster mean / baseline mean, unified
//     for both directions; a cluster is anomalous when its score exceeds the
//     ratio threshold (max direction) or falls below its reciprocal (min
//     direction, e.g. < 0.5 with the default threshold 2.0).
//  7. No anomalous cluster → exit.
//  8. Recurse into anomalous clusters (depth ≤ 10); a deeper anomaly replaces
//     the parent cluster, deeper silence keeps the parent.
//  9. Return the deepest anomalous clusters.
package clustering

import (
	"math"
	"math/rand"
)

// maxDepth bounds the recursive cluster refinement.
const maxDepth = 10

// maxIter bounds the Lloyd iteration count.
const maxIter = 300

// kmeansSeed fixes the D²-sampling stream so results are reproducible across
// runs (the user accepts the algorithm's inherent non-determinism; a fixed
// seed keeps single-run and test behavior stable).
const kmeansSeed int64 = 42

// Result is one detected anomalous data point.
type Result struct {
	Index int     // index into the original input values
	Ratio float64 // cluster mean / baseline mean, unified for both directions (max side: > 1, worse when larger; min side: < 1, worse when smaller)
}

// Detect finds anomalous points in values using recursive kmeans ratio
// detection. highIsAnomaly selects the direction: true means larger is worse
// (baseline = min-mean cluster), false means smaller is worse (baseline =
// max-mean cluster). Values ≤ 0 are ignored.
func Detect(values []float64, ratioThreshold float64, highIsAnomaly bool) []Result {
	idx, vals := filterPositive(values)
	if len(vals) < 2 {
		return nil
	}
	return detectRec(vals, idx, ratioThreshold, highIsAnomaly, 0)
}

// DiagnoseEntry is one data point's diagnostic at a single kmeans level.
type DiagnoseEntry struct {
	Index   int     // index into the original input values
	Value   float64 // original value
	Cluster int     // cluster id at this level (-1 when the value was filtered out)
	Ratio   float64 // ratio to the baseline cluster mean (0 when not on the anomaly side)
	Flagged bool    // ratio > ratioThreshold
}

// Diagnose runs one top-level kmeans level on values and reports EVERY point's
// cluster, ratio and flag — the debug counterpart of Detect, showing why a
// point was or wasn't flagged. Values ≤ 0 are filtered (Cluster = -1, Ratio 0).
// Ratio is cluster mean / baseline mean for every point (normal points sit
// near 1.0, filtered 0); Flagged is true when the ratio exceeds the ratio
// threshold on the max side, or falls below its reciprocal on the min side.
// It does not recurse, so the flagged set is the first-level decision only.
func Diagnose(values []float64, ratioThreshold float64, highIsAnomaly bool) []DiagnoseEntry {
	entries := make([]DiagnoseEntry, len(values))
	for i, v := range values {
		entries[i] = DiagnoseEntry{Index: i, Value: v, Cluster: -1}
	}
	idx, vals := filterPositive(values)
	if len(vals) < 2 {
		return entries
	}

	z := zscore(vals)
	k := elbowK(z)
	clusters := kmeans(z, k)
	baseIdx := pickBaselineCluster(clusters, vals, highIsAnomaly)
	baseMean := clusterMean(clusters[baseIdx], vals)
	if baseMean <= 0 {
		baseMean = math.SmallestNonzeroFloat64
	}

	// Cluster id and mean per original index.
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
		m := clusterMeans[cid]
		// Unified score: cluster mean / baseline mean for BOTH directions.
		// The anomaly side differs: max direction flags ratios ABOVE the
		// threshold, min direction flags ratios BELOW its reciprocal (e.g.
		// < 0.5 with the default threshold 2.0).
		ratio := m / baseMean
		entries[i].Ratio = ratio
		if highIsAnomaly {
			entries[i].Flagged = ratio > ratioThreshold
		} else {
			entries[i].Flagged = ratio < 1.0/ratioThreshold
		}
	}
	return entries
}

// filterPositive keeps values > 0 and their original indices (NaN excluded).
func filterPositive(values []float64) (idx []int, vals []float64) {
	for i, v := range values {
		if v > 0 {
			idx = append(idx, i)
			vals = append(vals, v)
		}
	}
	return idx, vals
}

// detectRec runs one kmeans level and recurses into anomaly clusters.
// indices[i] is the original index of vals[i]; cluster means are computed on
// the ORIGINAL values (not the standardized ones).
func detectRec(vals []float64, indices []int, threshold float64, highIsAnomaly bool, depth int) []Result {
	if len(vals) < 2 || depth > maxDepth {
		return nil
	}

	z := zscore(vals)
	k := elbowK(z)
	clusters := kmeans(z, k)
	baseIdx := pickBaselineCluster(clusters, vals, highIsAnomaly)
	baseMean := clusterMean(clusters[baseIdx], vals)
	if baseMean <= 0 {
		baseMean = math.SmallestNonzeroFloat64
	}

	// Step 6-7: baseline = direction extreme cluster. Score = cluster mean /
	// baseline mean (unified for both directions); max direction flags scores
	// ABOVE the threshold, min direction flags scores BELOW its reciprocal
	// (e.g. < 0.5 with the default threshold 2.0).
	var anomalyClusters [][]int
	var anomalyMeans []float64
	for i, cl := range clusters {
		if i == baseIdx {
			continue
		}
		m := clusterMean(cl, vals)
		var anomalous bool
		if highIsAnomaly {
			anomalous = m > baseMean && m/baseMean > threshold
		} else {
			anomalous = m < baseMean && m/baseMean < 1.0/threshold
		}
		if anomalous {
			anomalyClusters = append(anomalyClusters, cl)
			anomalyMeans = append(anomalyMeans, m)
		}
	}
	if len(anomalyClusters) == 0 {
		return nil
	}

	// Step 8-9: recurse into each anomaly cluster; deeper anomalies replace the
	// parent, deeper silence keeps the parent cluster's members.
	var results []Result
	for i, cl := range anomalyClusters {
		subVals := make([]float64, len(cl))
		subIdx := make([]int, len(cl))
		for j, li := range cl {
			subVals[j] = vals[li]
			subIdx[j] = indices[li]
		}
		deeper := detectRec(subVals, subIdx, threshold, highIsAnomaly, depth+1)
		if len(deeper) > 0 {
			results = append(results, deeper...)
			continue
		}
		ratio := anomalyMeans[i] / baseMean
		for _, li := range cl {
			results = append(results, Result{Index: indices[li], Ratio: ratio})
		}
	}
	return results
}

// zscore standardizes vals to zero mean / unit std; a near-zero std is forced
// to 1 (all-identical input becomes all-zero z-scores).
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
	if std <= 1e-9 {
		std = 1
	}
	z := make([]float64, n)
	for i, v := range vals {
		z[i] = (v - mean) / std
	}
	return z
}

// elbowK picks the number of clusters via the elbow method: k = 1..min(n,10)
// inertias are computed, and the k in [2, min(n,10)) with the largest second
// difference is selected.
func elbowK(z []float64) int {
	maxK := len(z)
	if maxK > 10 {
		maxK = 10
	}
	if maxK < 2 {
		return 1
	}
	inertias := make([]float64, maxK+1)
	for k := 1; k <= maxK; k++ {
		inertias[k] = kmeansInertia(z, k)
	}
	bestK := 2
	bestD2 := math.Inf(-1)
	for k := 2; k < maxK; k++ {
		d2 := inertias[k-1] - 2*inertias[k] + inertias[k+1]
		if d2 > bestD2 {
			bestD2 = d2
			bestK = k
		}
	}
	if bestK > maxK {
		bestK = maxK
	}
	return bestK
}

// kmeansInertia is the within-cluster sum of squared distances for a k-cluster
// fit (used only by the elbow method).
func kmeansInertia(z []float64, k int) float64 {
	clusters := kmeans(z, k)
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
// Lloyd iteration. Empty clusters claim the point farthest from its assigned
// centroid.
func kmeans(z []float64, k int) [][]int {
	n := len(z)
	if n == 0 {
		return nil
	}
	if k < 1 {
		k = 1
	}
	if k > n {
		k = n
	}

	// kmeans++ initialization: first centroid = z[0], subsequent centroids are
	// D²-weighted sampled.
	rng := rand.New(rand.NewSource(kmeansSeed))
	centroids := []float64{z[0]}
	for len(centroids) < k {
		distSq := make([]float64, n)
		var total float64
		for i := range z {
			d := z[i] - nearestCentroid(z[i], centroids)
			distSq[i] = d * d
			total += distSq[i]
		}
		chosen := 0
		if total <= 0 {
			// All points coincide with an existing centroid → no distinct
			// centroid can be added; stop early (all-identical input).
			break
		}
		target := rng.Float64() * total
		var cum float64
		chosen = n - 1
		for i := range z {
			cum += distSq[i]
			if cum >= target {
				chosen = i
				break
			}
		}
		centroids = append(centroids, z[chosen])
	}
	k = len(centroids)

	// Lloyd iteration.
	labels := make([]int, n)
	for i := range z {
		labels[i] = nearestCentroidIndex(z[i], centroids)
	}
	for iter := 0; iter < maxIter; iter++ {
		sums := make([]float64, k)
		counts := make([]int, k)
		for i, l := range labels {
			sums[l] += z[i]
			counts[l]++
		}
		// Empty clusters: claim the point farthest from its assigned centroid.
		for c := 0; c < k; c++ {
			if counts[c] == 0 {
				bestPt, bestDist := -1, -1.0
				for i := range z {
					d := math.Abs(z[i] - centroids[labels[i]])
					if d > bestDist {
						bestDist, bestPt = d, i
					}
				}
				if bestPt >= 0 {
					old := labels[bestPt]
					counts[old]--
					sums[old] -= z[bestPt]
					labels[bestPt] = c
					counts[c] = 1
					sums[c] = z[bestPt]
				}
			}
		}
		newCentroids := make([]float64, k)
		for c := 0; c < k; c++ {
			if counts[c] > 0 {
				newCentroids[c] = sums[c] / float64(counts[c])
			} else {
				newCentroids[c] = centroids[c]
			}
		}
		// Reassign and check convergence (1e-9 centroid movement + no label
		// changes).
		changed := false
		for i := range z {
			nl := nearestCentroidIndex(z[i], newCentroids)
			if nl != labels[i] {
				changed = true
			}
			labels[i] = nl
		}
		moved := false
		for c := 0; c < k; c++ {
			if math.Abs(newCentroids[c]-centroids[c]) > 1e-9 {
				moved = true
			}
		}
		centroids = newCentroids
		if !changed && !moved {
			break
		}
	}

	// Group by label.
	clusters := make([][]int, k)
	for i, l := range labels {
		clusters[l] = append(clusters[l], i)
	}
	return clusters
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
// cluster. Cluster means use the ORIGINAL values.
func pickBaselineCluster(clusters [][]int, vals []float64, highIsAnomaly bool) int {
	if len(clusters) == 0 {
		return -1
	}
	best := 0
	bestMean := clusterMean(clusters[0], vals)
	for i := 1; i < len(clusters); i++ {
		m := clusterMean(clusters[i], vals)
		if (highIsAnomaly && m < bestMean) || (!highIsAnomaly && m > bestMean) {
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
