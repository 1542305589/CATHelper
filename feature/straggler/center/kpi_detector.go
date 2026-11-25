package center

import (
	"context"
	"math"
	"math/rand"
	"sort"
	"strconv"
	"sync"
	"time"
)

const (
	defaultKPIDetectInterval = 10 * time.Second
	kpiDetectTick            = time.Second
	kpiAggregationWindowSec  = 30
	kpiReferenceLimit        = 50000
	kpiMinReferencePoints    = 100
	kpiSpaceMinDevices       = 3
	kpiActiveRunningMin      = 1.0
	kpiActiveUtilMedianPct   = 25.0
	kpiEnableTimeDimension   = false
	kpiSampleRetention       = 48 * time.Hour
	kpiAnomalyRetention      = 30 * 24 * time.Hour
	vllmMetricsRetention     = 7 * 24 * time.Hour
)

// kpiMetricSpec defines one detected metric. absolute=true means |z| > n is
// anomalous; absolute=false means only z < -n (low-side) is anomalous.
// timeN is retained for the (disabled) time dimension; space detection uses the
// multi-baseline band below instead of these z-score fields.
type kpiMetricSpec struct {
	name        string
	timeN       float64
	spaceN      float64
	madFloor    float64
	madFloorPct float64
	absolute    bool
}

var kpiMetricSpecs = []kpiMetricSpec{
	{name: "power", timeN: 4, spaceN: 4, madFloor: 5, absolute: true},
	{name: "aicore_util", timeN: 4, spaceN: 4, madFloor: 5, absolute: true},
	{name: "aicore_freq", timeN: 3, spaceN: 3, madFloor: 5, madFloorPct: 0.0675, absolute: false},
}

type kpiStats struct {
	median float64
	mad    float64
}

// kpiDetector owns the in-memory reference-invalid guard. Reference data itself is
// always queried from kpi_samples; there is no separate baseline database.
type kpiDetector struct {
	store   *kpiStore
	metrics *metricsStore
	logf    func(format string, args ...any)

	mu           sync.Mutex
	lastDetected map[string]int64 // business|node -> last detection ts
}

func newKPIDetector(store *kpiStore, metrics *metricsStore, logf func(format string, args ...any)) *kpiDetector {
	return &kpiDetector{
		store:        store,
		metrics:      metrics,
		logf:         logf,
		lastDetected: make(map[string]int64),
	}
}

// kpiCycleResult is one business detection cycle's outcome.
type kpiCycleResult struct {
	Detected    bool
	RealAnomaly bool
	LoadState   string
}

type kpiDueBusiness struct {
	name          string
	interval      time.Duration
	ns            KPINSigma
	refAdmitRatio float64
}

func (c *Center) kpiRetentionLoop(ctx context.Context) {
	if c.kpi == nil {
		return
	}
	ticker := time.NewTicker(time.Hour)
	defer ticker.Stop()
	for {
		if n, err := c.kpi.PruneVLLMMetrics(vllmMetricsRetention); err != nil {
			c.logf("vLLM metrics retention: %v", err)
		} else if n > 0 {
			c.logf("vLLM metrics retention: deleted %d rows (>=7d)", n)
		}
		if n, err := c.kpi.PruneKPI(kpiSampleRetention, kpiAnomalyRetention); err != nil {
			c.logf("KPI retention: %v", err)
		} else if n > 0 {
			c.logf("KPI retention: deleted %d rows (samples>=48h, anomalies>=30d; runs kept)", n)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (c *Center) kpiDetectLoop(ctx context.Context) {
	if c.kpiDet == nil {
		return
	}
	ticker := time.NewTicker(kpiDetectTick)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			for _, due := range c.dueKPIBusinesses(now) {
				go func(due kpiDueBusiness) {
					defer c.finishKPIDetection(due.name, due.interval)
					res, err := c.kpiDet.runForBusiness(due.name, due.interval, due.ns, due.refAdmitRatio)
					if err != nil {
						c.logf("KPI detection: business=%s: %v", due.name, err)
						return
					}
					if !res.Detected {
						return
					}
					c.recordKPIAnomaly(due.name, res.RealAnomaly, res.LoadState)
				}(due)
			}
		}
	}
}

func (c *Center) dueKPIBusinesses(now time.Time) []kpiDueBusiness {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]kpiDueBusiness, 0, len(c.biz))
	changed := false
	for name, b := range c.biz {
		interval := kpiBusinessInterval(b, c.cfg.KPIDetectInterval)
		if b.NextKPIDetect.IsZero() {
			b.NextKPIDetect = now.Add(interval)
			changed = true
		}
		if now.Before(b.NextKPIDetect) || b.kpiDetecting {
			continue
		}
		b.kpiDetecting = true
		out = append(out, kpiDueBusiness{name: name, interval: interval, ns: b.KPINSigma.Normalized(), refAdmitRatio: b.NormalizedKPIReferenceAdmitRatio()})
	}
	if changed {
		c.save()
	}
	return out
}

func (c *Center) finishKPIDetection(name string, interval time.Duration) {
	c.mu.Lock()
	if b := c.biz[name]; b != nil {
		b.kpiDetecting = false
		b.NextKPIDetect = time.Now().Add(interval)
		c.save()
	}
	c.mu.Unlock()
}

func kpiBusinessInterval(b *Business, def time.Duration) time.Duration {
	if b != nil && b.KPIDetectIntervalSec >= 5 {
		return time.Duration(b.KPIDetectIntervalSec) * time.Second
	}
	if def >= 5 {
		return def
	}
	return defaultKPIDetectInterval
}

func (d *kpiDetector) runForBusiness(business string, interval time.Duration, ns KPINSigma, refAdmitRatio float64) (kpiCycleResult, error) {
	var res kpiCycleResult
	load := loadSnapshot{}
	if d.metrics != nil {
		load = d.metrics.LoadSnapshot(business, time.Now().Unix())
	}
	nodes, err := d.store.NodesDevices(business)
	if err != nil {
		return res, err
	}
	filters, err := d.store.Filters(business)
	if err != nil {
		return res, err
	}
	byNode := make(map[string]kpiFilter, len(filters))
	for _, f := range filters {
		byNode[f.Node] = f
	}

	for node, devices := range nodes {
		latest, err := d.store.LatestSampleTs(business, node)
		if err != nil {
			d.logf("KPI detection: latest ts %s/%s: %v", business, node, err)
			continue
		}
		if latest == 0 || latest <= d.lastRun(business, node) {
			continue
		}
		d.setLastRun(business, node, latest)
		now := latest
		windowSec := int64(interval.Seconds())
		if windowSec < 5 {
			windowSec = int64(defaultKPIDetectInterval.Seconds())
		}
		since := latest - windowSec
		filter := byNode[node]
		if filter.FilterAll {
			continue
		}
		allowed := filterDevices(devices, filter.Devices)
		if len(allowed) == 0 {
			continue
		}
		samples, err := d.store.RecentSamples(business, node, since)
		if err != nil {
			d.logf("KPI detection: recent samples %s/%s: %v", business, node, err)
			continue
		}
		current := aggregateCurrent(samples, allowed)
		if len(current) == 0 {
			continue
		}
		utilValues := make([]float64, 0, len(current))
		for _, metrics := range current {
			if value, ok := metrics["aicore_util"]; ok {
				utilValues = append(utilValues, value)
			}
		}
		nodeLoadState := classifyLoad(load, median(utilValues))
		res.LoadState = mergeLoadState(res.LoadState, nodeLoadState)

		res.Detected = true
		var anomalies []kpiAnomaly

		// Space dimension only: per metric, cluster the current round's device
		// values into baselines and flag values outside the 0.7-1.3 band. No
		// cross-time comparison is performed.
		referenceInvalid := false
		refSince := since - int64(kpiSampleRetention.Seconds())
		if refCount, err := d.store.ReferenceCount(business, node, refSince, since, windowSec); err != nil {
			d.logf("KPI detection: reference count %s/%s: %v", business, node, err)
		} else if refCount < kpiMinReferencePoints {
			referenceInvalid = true
		}
		if len(current) >= kpiSpaceMinDevices {
			for _, spec := range kpiMetricSpecs {
				values := make(map[int]float64)
				for device, metrics := range current {
					if value, ok := metrics[spec.name]; ok {
						values[device] = value
					}
				}
				if len(values) < kpiSpaceMinDevices {
					continue
				}
				_, bandAnomalies := DetectMajorBand(values)
				for _, a := range bandAnomalies {
					anomalies = append(anomalies, kpiAnomaly{
						Business: business, Node: node, Device: a.Device, Ts: now,
						Metric: spec.name, Kind: "space", Value: a.Value,
						Median: a.Center, Center: a.Center, Ratio: a.Ratio,
					})
				}
			}
		}

		if len(anomalies) > 0 && refAdmitRatio > 0 {
			admit := make(map[string]bool)
			for i := range anomalies {
				a := &anomalies[i]
				key := a.Node + "|" + strconv.Itoa(a.Device) + "|" + strconv.FormatInt(a.Ts, 10)
				ok, seen := admit[key]
				if !seen {
					ok = rand.Float64() < refAdmitRatio
					admit[key] = ok
				}
				a.ReferenceAdmitted = ok
			}
		}

		if err := d.store.InsertAnomalies(anomalies); err != nil {
			d.logf("KPI detection: store anomalies %s/%s: %v", business, node, err)
			anomalies = nil
		}

		flagged := make(map[int]bool)
		for _, a := range anomalies {
			flagged[a.Device] = true
		}
		// Only active-node anomalies count toward the profiler-trigger streak;
		// not_active / unknown rounds are recorded but never trigger.
		if nodeLoadState == "active" && len(anomalies) > 0 {
			res.RealAnomaly = true
		}

		if err := d.store.InsertDetectRun(kpiDetectRun{
			Business: business, Node: node, Ts: now, Devices: len(current),
			Anomalies: len(anomalies), Flagged: len(flagged),
			ReferenceInvalid: referenceInvalid,
			LoadState:        nodeLoadState, Reqs5m: load.Reqs5m, RunningP90: load.RunningP90,
			LoadRatio: load.LoadRatio, MetricsFresh: load.Fresh,
		}); err != nil {
			d.logf("KPI detection: store run %s/%s: %v", business, node, err)
		}
		if len(anomalies) > 0 {
			d.logf("KPI detection: %s/%s anomalies=%d devices=%d", business, node, len(anomalies), len(flagged))
		}
	}
	if res.LoadState == "" {
		res.LoadState = "unknown"
	}
	return res, nil
}

// mergeLoadState combines per-node activity states for the business-level
// decision: any active node makes the business active; otherwise any unknown
// keeps it unknown; only all-not_active yields not_active.
func mergeLoadState(cur, next string) string {
	switch {
	case cur == "":
		return next
	case cur == "active" || next == "active":
		return "active"
	case cur == "unknown" || next == "unknown":
		return "unknown"
	default:
		return "not_active"
	}
}

func (d *kpiDetector) lastRun(business, node string) int64 {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.lastDetected[kpiNodeKey(business, node)]
}

func (d *kpiDetector) setLastRun(business, node string, ts int64) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.lastDetected[kpiNodeKey(business, node)] = ts
}

func kpiNodeKey(business, node string) string { return business + "|" + node }

func filterDevices(devices, allow []int) []int {
	if len(allow) == 0 {
		return devices
	}
	allowed := make(map[int]bool, len(allow))
	for _, device := range allow {
		allowed[device] = true
	}
	out := make([]int, 0, len(devices))
	for _, device := range devices {
		if allowed[device] {
			out = append(out, device)
		}
	}
	return out
}

func aggregateCurrent(samples []kpiStoredSample, allow []int) map[int]map[string]float64 {
	allowed := make(map[int]bool, len(allow))
	for _, device := range allow {
		allowed[device] = true
	}
	grouped := make(map[int]map[string][]float64)
	for _, sample := range samples {
		if len(allowed) > 0 && !allowed[sample.Device] {
			continue
		}
		if grouped[sample.Device] == nil {
			grouped[sample.Device] = make(map[string][]float64)
		}
		for _, spec := range kpiMetricSpecs {
			if value, ok := sample.Metrics[spec.name]; ok {
				grouped[sample.Device][spec.name] = append(grouped[sample.Device][spec.name], value)
			}
		}
	}
	out := make(map[int]map[string]float64, len(grouped))
	for device, metrics := range grouped {
		values := make(map[string]float64, len(metrics))
		for metric, list := range metrics {
			values[metric] = midmean(list)
		}
		if len(values) > 0 {
			out[device] = values
		}
	}
	return out
}

// aggregateReference groups raw reference samples into the same window buckets as
// the current point, so time-dimension median/MAD compares like with like.
func aggregateReference(ref []kpiRefPoint, windowSec int64) []kpiRefPoint {
	if windowSec <= 0 {
		windowSec = int64(defaultKPIDetectInterval.Seconds())
	}
	type bucket struct {
		ts     int64
		values map[string][]float64
	}
	buckets := make(map[int64]*bucket)
	for _, point := range ref {
		bucketTS := (point.Ts / windowSec) * windowSec
		b := buckets[bucketTS]
		if b == nil {
			b = &bucket{ts: bucketTS, values: make(map[string][]float64)}
			buckets[bucketTS] = b
		}
		for _, spec := range kpiMetricSpecs {
			if value, ok := point.Metrics[spec.name]; ok {
				b.values[spec.name] = append(b.values[spec.name], value)
			}
		}
	}
	out := make([]kpiRefPoint, 0, len(buckets))
	for _, b := range buckets {
		metrics := make(map[string]float64)
		for metric, values := range b.values {
			metrics[metric] = midmean(values)
		}
		if len(metrics) > 0 {
			out = append(out, kpiRefPoint{Ts: b.ts, Metrics: metrics})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Ts < out[j].Ts })
	return out
}

func computeMetricStats(ref []kpiRefPoint) map[string]kpiStats {
	out := make(map[string]kpiStats)
	for _, spec := range kpiMetricSpecs {
		values := make([]float64, 0, len(ref))
		for _, point := range ref {
			if value, ok := point.Metrics[spec.name]; ok {
				values = append(values, value)
			}
		}
		if len(values) == 0 {
			continue
		}
		median, mad := medianMAD(values)
		mad = effectiveMAD(mad, spec.madFloor)
		out[spec.name] = kpiStats{median: median, mad: mad}
	}
	return out
}

func isKPIAnomaly(z, n float64, absolute bool) bool {
	if absolute {
		return math.Abs(z) > n
	}
	return z < -n
}

func medianMAD(values []float64) (medianValue, mad float64) {
	if len(values) == 0 {
		return 0, 0
	}
	medianValue = median(values)
	deviations := make([]float64, len(values))
	for i, value := range values {
		deviations[i] = math.Abs(value - medianValue)
	}
	return medianValue, median(deviations)
}

func effectiveMAD(mad, madFloor float64) float64 {
	if madFloor > 0 && mad < madFloor {
		return madFloor
	}
	return mad
}

func modifiedZ(value, medianValue, mad, madFloor float64) float64 {
	if madFloor <= 0 {
		madFloor = math.Max(math.Abs(medianValue)*1e-6, 1e-9)
	}
	if mad < madFloor {
		mad = madFloor
	}
	return 0.6745 * (value - medianValue) / mad
}

func median(values []float64) float64 {
	if len(values) == 0 {
		return 0
	}
	sorted := append([]float64(nil), values...)
	sort.Float64s(sorted)
	n := len(sorted)
	if n%2 == 1 {
		return sorted[n/2]
	}
	return (sorted[n/2-1] + sorted[n/2]) / 2
}

func midmean(values []float64) float64 {
	if len(values) == 0 {
		return 0
	}
	sorted := append([]float64(nil), values...)
	sort.Float64s(sorted)
	if len(sorted) < 4 {
		sum := 0.0
		for _, value := range sorted {
			sum += value
		}
		return sum / float64(len(sorted))
	}
	trim := len(sorted) / 4
	keep := sorted[trim : len(sorted)-trim]
	if len(keep) < 2 {
		return sorted[len(sorted)/2]
	}
	sum := 0.0
	for _, value := range keep {
		sum += value
	}
	return sum / float64(len(keep))
}

func allEqual(values []float64) bool {
	if len(values) < 2 {
		return true
	}
	first := values[0]
	for _, value := range values[1:] {
		if value != first {
			return false
		}
	}
	return true
}

func valuesToSlice(values map[int]float64) []float64 {
	out := make([]float64, 0, len(values))
	for _, value := range values {
		out = append(out, value)
	}
	return out
}

// recordKPIAnomaly updates the per-business anomaly streak and triggers a
// profiler run after three consecutive anomaly runs, with a 10-minute cooldown.
func (c *Center) recordKPIAnomaly(business string, hadAnomaly bool, loadState string) {
	c.mu.Lock()
	b := c.biz[business]
	if b == nil {
		c.mu.Unlock()
		return
	}
	if !hadAnomaly || loadState != "active" {
		b.kpiAnomalyStreak = 0
		c.mu.Unlock()
		return
	}
	b.kpiAnomalyStreak++
	if b.kpiAnomalyStreak < 3 {
		c.mu.Unlock()
		return
	}
	if b.Paused || b.triggering || time.Now().Before(b.kpiProfilerCooldownUntil) {
		c.mu.Unlock()
		return
	}
	b.kpiProfilerCooldownUntil = time.Now().Add(10 * time.Minute)
	b.kpiAnomalyStreak = 0
	c.mu.Unlock()
	c.logf("business %s: 3 consecutive KPI anomaly runs; triggering profiler (cooldown 10m)", business)
	go c.triggerBusinessFromKPI(b)
}
