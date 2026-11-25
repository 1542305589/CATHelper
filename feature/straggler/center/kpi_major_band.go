package center

import (
	"sort"

	"github.com/Computing-Availability-Tools/CATHelper/feature/straggler/clustering"
)

// kpiBandAnomaly is one device value outside every major-baseline band.
type kpiBandAnomaly struct {
	Device int
	Value  float64
	Center float64 // nearest baseline cluster center (by ratio distance)
	Ratio  float64 // Value / Center
}

// DetectMajorBand maps device values onto the shared clustering.DetectMajorBand
// primitive. The clustering implementation owns the elbow-K / kmeans++ /
// 0.7-1.3 band policy so the center and any other caller cannot drift.
func DetectMajorBand(values map[int]float64) ([]float64, []kpiBandAnomaly) {
	if len(values) < kpiSpaceMinDevices {
		return nil, nil
	}
	devices := make([]int, 0, len(values))
	for device := range values {
		devices = append(devices, device)
	}
	sort.Ints(devices)
	raw := make([]float64, len(devices))
	for i, device := range devices {
		raw[i] = values[device]
	}
	baselines, results := clustering.DetectMajorBand(raw)
	if len(results) == 0 {
		return baselines, nil
	}
	anomalies := make([]kpiBandAnomaly, 0, len(results))
	for _, r := range results {
		anomalies = append(anomalies, kpiBandAnomaly{
			Device: devices[r.Index],
			Value:  r.Value,
			Center: r.Center,
			Ratio:  r.Ratio,
		})
	}
	return baselines, anomalies
}
