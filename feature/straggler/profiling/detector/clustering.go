// Package detector implements the straggler (slow-node) detection pipeline.
//
// HomogenizationComparisonFunc is the public entry point for the shared kmeans
// ratio detection algorithm (see the clustering package): a recursive 1-D
// kmeans detector that finds abnormal data points by selecting the direction
// extreme cluster as the baseline and comparing cluster mean ratios.
//
// It is the single anomaly-detection primitive used by all four detection
// categories (slow compute, slow communication, slow CPU, NPU bubble).
package detector

import (
	"github.com/Computing-Availability-Tools/CATHelper/feature/straggler/clustering"
)

// HomogenizationComparisonFunc is the public entry point.
//
// Parameters:
//   - fileRanks:           original rank IDs (e.g. [1, 5, 9, 13])
//   - alignedData:         data values, one per rank
//   - degradationPercent:  ratio threshold (e.g. 1.3 for compute, 2.5 for communication)
//   - abnormalType:        "max" (bigger is worse) or "min" (smaller is worse)
//
// Returns:
//   - abnormal rank IDs
//   - corresponding degradation scores, unified as cluster mean / baseline
//     mean for both directions ("max": > 1, worse when larger; "min": < 1,
//     worse when smaller)
//
// It shares the KPI resource space detector's clustering algorithm — kmeans
// with the direction extreme cluster as baseline and cluster-mean-ratio
// significance — so both dimensions follow the same detection philosophy.
func HomogenizationComparisonFunc(
	fileRanks []int,
	alignedData []float64,
	degradationPercent float64,
	abnormalType string,
) ([]int, []float64) {
	if len(alignedData) == 0 || len(fileRanks) == 0 || len(alignedData) != len(fileRanks) {
		return nil, nil
	}
	if len(alignedData) < 2 {
		return nil, nil
	}

	results := clustering.Detect(alignedData, degradationPercent, abnormalType != "min")

	abnormalRanks := make([]int, 0, len(results))
	degradations := make([]float64, 0, len(results))
	for _, r := range results {
		abnormalRanks = append(abnormalRanks, fileRanks[r.Index])
		degradations = append(degradations, r.Ratio)
	}
	return abnormalRanks, degradations
}
