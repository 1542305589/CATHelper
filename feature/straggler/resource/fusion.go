package resource

import (
	"fmt"
	"os"
	"sort"
)

// =============================================================================
// Fusion: Space-Only Fusion + Compute-First Ordering
// =============================================================================

// FuseAndSummarize aggregates the space detection details and enforces the
// "compute first, communication second" detection order.
//
// For each card:
//  1. Check compute metrics first.
//  2. If compute anomaly found → category=compute, communication metrics
//     are checked but flagged as "secondary" (possibly consequential).
//  3. If compute is clean → check communication metrics → if anomalous,
//     category=communication (independent network issue).
func FuseAndSummarize(
	spaceDetails map[int]map[MetricName]*MetricAnomalyDetail,
	cardIDs []int,
	cfg DetectionConfig,
) []CardDetectionSummary {
	var summaries []CardDetectionSummary

	for _, cid := range cardIDs {
		summary := fuseOneCard(cid, spaceDetails[cid], cfg)
		summaries = append(summaries, summary)
	}

	// Sort: confirmed anomalies first, then by composite score descending.
	sort.Slice(summaries, func(i, j int) bool {
		if summaries[i].Quadrant != summaries[j].Quadrant {
			// Confirmed anomalies first.
			qi := quadrantOrder(summaries[i].Quadrant)
			qj := quadrantOrder(summaries[j].Quadrant)
			return qi < qj
		}
		return summaries[i].CompositeScore > summaries[j].CompositeScore
	})

	return summaries
}

// quadrantOrder returns a sort order for quadrants (lower = more severe).
func quadrantOrder(q Quadrant) int {
	switch q {
	case QuadConfirmedAnomaly:
		return 0
	case QuadEarlyDegradation:
		return 1
	case QuadIndividualVariance:
		return 2
	default:
		return 3
	}
}

// fuseOneCard fuses the space detection details for a single card with
// compute-first logic. Anomaly is decided purely by the space dimension.
func fuseOneCard(
	cid int,
	spaceM map[MetricName]*MetricAnomalyDetail,
	cfg DetectionConfig,
) CardDetectionSummary {
	// Step 1: Check compute metrics.
	hasComputeAnomaly := false
	for _, metric := range AllMetrics {
		if IsComputeMetric(metric) {
			if d, ok := spaceM[metric]; ok && d.SpaceAbnormal {
				hasComputeAnomaly = true
			}
		}
	}

	var summary CardDetectionSummary
	summary.CardID = cid

	// Debug (--debug-output): show every metric's full space detail, even
	// normal ones (space_abnormal/quadrant false/0), so undetected metrics can
	// be inspected alongside the flags.
	if cfg.EnableDebug {
		summary.AnomalyDetails = make([]MetricAnomalyDetail, 0, len(AllMetrics))
		for _, metric := range AllMetrics {
			d := spaceM[metric]
			d.determineQuadrant()
			summary.AnomalyDetails = append(summary.AnomalyDetails, *d)
		}
		if hasComputeAnomaly {
			summary.AnomalyCategory = CatCompute
		} else {
			for _, d := range summary.AnomalyDetails {
				if IsCommunicationMetric(d.Metric) && d.SpaceAbnormal {
					summary.AnomalyCategory = CatCommunication
					break
				}
			}
		}
		summary.Quadrant = worstQuadrant(summary.AnomalyDetails)
		summary.CompositeScore = compositeScore(summary.AnomalyDetails)
		summary.Severity = determineSeverity(summary.Quadrant, summary.CompositeScore)
		fmt.Fprintf(os.Stderr, "[SLOWNODE ALGO] Card %d: category=%s quadrant=%s score=%.2f anomalies=%d\n",
			cid, summary.AnomalyCategory, summary.Quadrant, summary.CompositeScore, len(summary.AnomalyDetails))
		return summary
	}

	if hasComputeAnomaly {
		// Compute anomaly: category=compute.
		// Check communication metrics but flag as secondary.
		summary.AnomalyCategory = CatCompute

		for _, metric := range AllMetrics {
			d := spaceM[metric]
			d.determineQuadrant()
			if IsComputeMetric(metric) {
				if d.SpaceAbnormal {
					summary.AnomalyDetails = append(summary.AnomalyDetails, *d)
				}
			} else if IsCommunicationMetric(metric) {
				if d.SpaceAbnormal {
					// Communication anomaly on a compute-anomalous card:
					// likely secondary — flag separately.
					summary.SecondaryCommAnomalies = append(summary.SecondaryCommAnomalies, *d)
				}
			}
		}

		// Determine overall quadrant from compute metrics only.
		summary.Quadrant = worstQuadrant(summary.AnomalyDetails)
		summary.CompositeScore = compositeScore(summary.AnomalyDetails)
		summary.Severity = determineSeverity(summary.Quadrant, summary.CompositeScore)

	} else {
		// Compute clean → check communication.
		summary.AnomalyCategory = CatNone

		// Check all metrics.
		for _, metric := range AllMetrics {
			d := spaceM[metric]
			d.determineQuadrant()
			if d.SpaceAbnormal {
				summary.AnomalyDetails = append(summary.AnomalyDetails, *d)
			}
		}

		// Determine category from anomalous metrics.
		hasCommAnomaly := false
		for _, d := range summary.AnomalyDetails {
			if IsCommunicationMetric(d.Metric) {
				hasCommAnomaly = true
			}
		}
		if hasCommAnomaly {
			summary.AnomalyCategory = CatCommunication
		}

		summary.Quadrant = worstQuadrant(summary.AnomalyDetails)
		summary.CompositeScore = compositeScore(summary.AnomalyDetails)
		summary.Severity = determineSeverity(summary.Quadrant, summary.CompositeScore)
	}

	if len(summary.AnomalyDetails) == 0 && len(summary.SecondaryCommAnomalies) > 0 {
		// Only secondary comm anomalies: still flag as compute-related.
		summary.Quadrant = QuadConfirmedAnomaly
		summary.CompositeScore = compositeScore(summary.SecondaryCommAnomalies)
		summary.Severity = SevWarning
	}

	fmt.Fprintf(os.Stderr, "[SLOWNODE ALGO] Card %d: category=%s quadrant=%s score=%.2f anomalies=%d secondary=%d\n",
		cid, summary.AnomalyCategory, summary.Quadrant, summary.CompositeScore,
		len(summary.AnomalyDetails), len(summary.SecondaryCommAnomalies))

	return summary
}

// =============================================================================
// Detail Helpers
// =============================================================================

// determineQuadrant decides the anomaly state purely by the space dimension:
// space-abnormal → confirmed_anomaly, else normal.
func (d *MetricAnomalyDetail) determineQuadrant() {
	if d.SpaceAbnormal {
		d.Quadrant = QuadConfirmedAnomaly
	} else {
		d.Quadrant = QuadNormal
	}
}

// worstQuadrant returns the most severe quadrant from a list of details.
func worstQuadrant(details []MetricAnomalyDetail) Quadrant {
	worst := QuadNormal
	for _, d := range details {
		if quadrantOrder(d.Quadrant) < quadrantOrder(worst) {
			worst = d.Quadrant
		}
	}
	return worst
}

// compositeScore computes the mean space score across all provided details.
func compositeScore(details []MetricAnomalyDetail) float64 {
	if len(details) == 0 {
		return 0
	}
	var sum float64
	for _, d := range details {
		sum += d.SpaceScore
	}
	return sum / float64(len(details))
}

// determineSeverity maps quadrant + score to severity.
func determineSeverity(q Quadrant, score float64) Severity {
	switch q {
	case QuadConfirmedAnomaly:
		if score >= 5 {
			return SevCritical
		}
		return SevWarning
	case QuadEarlyDegradation:
		return SevInfo
	case QuadIndividualVariance:
		return SevInfo
	default:
		return SevInfo
	}
}
