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
	kpiReferenceLimit        = 10000
	kpiMinReferencePoints    = 100
	kpiSpaceMinDevices       = 3
	kpiFreqTolerance         = 0.01
)

// kpiMetricSpec defines one detected metric. absolute=true means |z| > n is
// anomalous; absolute=false means only z < -n (low-side) is anomalous.
type kpiMetricSpec struct {
	name     string
	timeN    float64
	spaceN   float64
	madFloor float64
	absolute bool
}

var kpiMetricSpecs = []kpiMetricSpec{
	{name: "power", timeN: 4, spaceN: 4, madFloor: 5, absolute: true},
	{name: "aicore_util", timeN: 4, spaceN: 4, madFloor: 5, absolute: true},
	{name: "aicore_freq", timeN: 3, spaceN: 3, madFloor: 5, absolute: false},
}

type kpiStats struct {
	median float64
	mad    float64
}

// kpiDetector owns the in-memory reference-invalid guard. Reference data itself is
// always queried from kpi_samples; there is no separate baseline database.
type kpiDetector struct {
	store *kpiStore
	logf  func(format string, args ...any)

	mu            sync.Mutex
	lastDetected  map[string]int64   // business|node -> last detection ts
	shiftCounts   map[string][]int64 // business|node|metric -> consecutive global-shift ts
	shiftCooldown map[string]int64   // business|node -> global-shift cooldown until
}

func newKPIDetector(store *kpiStore, logf func(format string, args ...any)) *kpiDetector {
	return &kpiDetector{
		store:         store,
		logf:          logf,
		lastDetected:  make(map[string]int64),
		shiftCounts:   make(map[string][]int64),
		shiftCooldown: make(map[string]int64),
	}
}

// kpiCycleResult is one business detection cycle's outcome.
type kpiCycleResult struct {
	Detected             bool
	RealAnomaly          bool
	GlobalShiftConfirmed bool
	GlobalShiftEvents    []kpiGlobalShiftEvent
}

// kpiGlobalShiftEvent is a confirmed global drift for one metric.
type kpiGlobalShiftEvent struct {
	Node   string
	Metric string
	TsList []int64
	Since  int64
}

type kpiDueBusiness struct {
	name           string
	interval       time.Duration
	ns             KPINSigma
	refAdmitRatio  float64
	referenceSince map[string]int64
}

func (c *Center) kpiRetentionLoop(ctx context.Context) {
	if c.kpi == nil {
		return
	}
	ticker := time.NewTicker(time.Hour)
	defer ticker.Stop()
	for {
		if n, err := c.kpi.PruneKPI(48 * time.Hour); err != nil {
			c.logf("KPI retention: %v", err)
		} else if n > 0 {
			c.logf("KPI retention: deleted %d rows older than 48h", n)
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
					res, err := c.kpiDet.runForBusiness(due.name, due.interval, due.ns, due.refAdmitRatio, due.referenceSince)
					if err != nil {
						c.logf("KPI detection: business=%s: %v", due.name, err)
						return
					}
					if len(res.GlobalShiftEvents) > 0 {
						c.applyGlobalShift(due.name, res.GlobalShiftEvents)
					}
					if !res.Detected {
						return
					}
					if res.GlobalShiftConfirmed {
						c.resetKPIAnomalyStreak(due.name)
					}
					if res.RealAnomaly {
						c.recordKPIAnomaly(due.name, true)
					} else if !res.GlobalShiftConfirmed {
						c.recordKPIAnomaly(due.name, false)
					}
				}(due)
			}
		}
	}
}

func (c *Center) dueKPIBusinesses(now time.Time) []kpiDueBusiness {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]kpiDueBusiness, 0, len(c.biz))
	for name, b := range c.biz {
		interval := kpiBusinessInterval(b, c.cfg.KPIDetectInterval)
		if b.nextKPIDetect.IsZero() {
			b.nextKPIDetect = now.Add(interval)
		}
		if now.Before(b.nextKPIDetect) || b.kpiDetecting {
			continue
		}
		b.kpiDetecting = true
		out = append(out, kpiDueBusiness{name: name, interval: interval, ns: b.KPINSigma.Normalized(), refAdmitRatio: b.NormalizedKPIReferenceAdmitRatio(), referenceSince: copyInt64Map(b.ReferenceSince)})
	}
	return out
}

func (c *Center) finishKPIDetection(name string, interval time.Duration) {
	c.mu.Lock()
	if b := c.biz[name]; b != nil {
		b.kpiDetecting = false
		b.nextKPIDetect = time.Now().Add(interval)
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

func (d *kpiDetector) runForBusiness(business string, interval time.Duration, ns KPINSigma, refAdmitRatio float64, referenceSince map[string]int64) (kpiCycleResult, error) {
	var res kpiCycleResult
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

		res.Detected = true
		referenceInvalid := false
		refSince := referenceSince[node]
		var anomalies []kpiAnomaly

		// Time dimension: each device against its own reference window.
		for device, metrics := range current {
			ref, err := d.store.ReferenceSamples(business, node, device, refSince, since, windowSec, kpiReferenceLimit)
			if err != nil {
				d.logf("KPI detection: reference %s/%s/device %d: %v", business, node, device, err)
				continue
			}
			ref = aggregateReference(ref, windowSec)
			ref = filterWorkingReference(ref, metrics["aicore_freq"])
			if len(ref) < kpiMinReferencePoints {
				referenceInvalid = true
				continue
			}
			stats := computeMetricStats(ref)
			for _, spec := range kpiMetricSpecs {
				value, ok := metrics[spec.name]
				if !ok {
					continue
				}
				st, ok := stats[spec.name]
				if !ok {
					continue
				}
				z := modifiedZ(value, st.median, st.mad, spec.madFloor)
				if isKPIAnomaly(z, ns.TimeN(spec.name), spec.absolute) {
					anomalies = append(anomalies, kpiAnomaly{
						Business: business, Node: node, Device: device, Ts: now,
						Metric: spec.name, Kind: "time", Value: value,
						Median: st.median, Mad: st.mad, Z: z,
					})
				}
			}
		}

		// Space dimension: same node, same detection window, per metric.
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
				median, mad := medianMAD(valuesToSlice(values))
				mad = effectiveMAD(mad, spec.madFloor)
				for device, value := range values {
					z := modifiedZ(value, median, mad, spec.madFloor)
					if isKPIAnomaly(z, ns.SpaceN(spec.name), spec.absolute) {
						anomalies = append(anomalies, kpiAnomaly{
							Business: business, Node: node, Device: device, Ts: now,
							Metric: spec.name, Kind: "space", Value: value,
							Median: median, Mad: mad, Z: z,
						})
					}
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

		// Store anomalies first; only successfully stored anomalies may drive
		// global-shift state, so a DB failure cannot create a 0/0 global_shift run.
		if err := d.store.InsertAnomalies(anomalies); err != nil {
			d.logf("KPI detection: store anomalies %s/%s: %v", business, node, err)
			anomalies = nil
		}

		// Global-shift detection: a metric flagged on >= half the devices for
		// 3 consecutive cycles is treated as an environment/workload shift.
		metricDevices := make(map[string]map[int]bool)
		for _, a := range anomalies {
			if metricDevices[a.Metric] == nil {
				metricDevices[a.Metric] = make(map[int]bool)
			}
			metricDevices[a.Metric][a.Device] = true
		}
		cooldownKey := business + "|" + node
		d.mu.Lock()
		shiftCooling := now < d.shiftCooldown[cooldownKey]
		d.mu.Unlock()
		for _, spec := range kpiMetricSpecs {
			if shiftCooling {
				continue
			}
			fm := metricDevices[spec.name]
			ratio := 0.0
			if len(current) > 0 {
				ratio = float64(len(fm)) / float64(len(current))
			}
			key := business + "|" + node + "|" + spec.name
			if len(fm) >= 3 && ratio >= 0.5 {
				d.mu.Lock()
				state := append(d.shiftCounts[key], now)
				if len(state) > 3 {
					state = state[len(state)-3:]
				}
				d.shiftCounts[key] = state
				confirmed := len(state) >= 3
				if confirmed {
					delete(d.shiftCounts, key)
					d.shiftCooldown[cooldownKey] = now + 600
				}
				d.mu.Unlock()
				if confirmed {
					res.GlobalShiftConfirmed = true
					res.GlobalShiftEvents = append(res.GlobalShiftEvents, kpiGlobalShiftEvent{
						Node: node, Metric: spec.name, TsList: state,
						Since: state[0] - windowSec,
					})
				}
			} else {
				d.mu.Lock()
				delete(d.shiftCounts, key)
				d.mu.Unlock()
			}
		}

		flagged := make(map[int]bool)
		for _, a := range anomalies {
			flagged[a.Device] = true
		}
		res.RealAnomaly = res.RealAnomaly || len(anomalies) > 0

		if err := d.store.InsertDetectRun(kpiDetectRun{
			Business: business, Node: node, Ts: now, Devices: len(current),
			Anomalies: len(anomalies), Flagged: len(flagged),
			ReferenceInvalid: referenceInvalid, GlobalShift: res.GlobalShiftConfirmed,
		}); err != nil {
			d.logf("KPI detection: store run %s/%s: %v", business, node, err)
		}
		if len(anomalies) > 0 {
			d.logf("KPI detection: %s/%s anomalies=%d devices=%d", business, node, len(anomalies), len(flagged))
		}
	}
	return res, nil
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

func filterWorkingReference(ref []kpiRefPoint, currentFreq float64) []kpiRefPoint {
	freqs := make([]float64, 0, len(ref))
	for _, point := range ref {
		if value, ok := point.Metrics["aicore_freq"]; ok {
			freqs = append(freqs, value)
		}
	}
	if len(freqs) == 0 {
		return ref
	}
	medianFreq := median(freqs)
	if allEqual(freqs) {
		return ref
	}
	freqWork := math.Max(medianFreq, currentFreq)
	threshold := freqWork * (1 - kpiFreqTolerance)
	out := make([]kpiRefPoint, 0, len(ref))
	for _, point := range ref {
		value, ok := point.Metrics["aicore_freq"]
		if !ok || value >= threshold {
			out = append(out, point)
		}
	}
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
func (c *Center) recordKPIAnomaly(business string, hadAnomaly bool) {
	c.mu.Lock()
	b := c.biz[business]
	if b == nil {
		c.mu.Unlock()
		return
	}
	if !hadAnomaly {
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

func copyInt64Map(in map[string]int64) map[string]int64 {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]int64, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func (c *Center) resetKPIAnomalyStreak(business string) {
	c.mu.Lock()
	if b := c.biz[business]; b != nil {
		b.kpiAnomalyStreak = 0
	}
	c.mu.Unlock()
}

// applyGlobalShift retroactively exempts the confirmed global-drift cycles,
// refreshes the node reference window, and records a per-metric global shift.
func (c *Center) applyGlobalShift(business string, events []kpiGlobalShiftEvent) {
	for _, e := range events {
		if e.Since <= 0 || len(e.TsList) == 0 {
			continue
		}
		latest := e.TsList[0]
		for _, ts := range e.TsList {
			if ts > latest {
				latest = ts
			}
		}
		if err := c.kpi.MarkRunsGlobalShift(business, e.Node, e.TsList); err != nil {
			c.logf("KPI global shift: mark runs %s/%s/%s: %v", business, e.Node, e.Metric, err)
		}
		if err := c.kpi.AdmitAnomaliesInRange(business, e.Node, e.Since, latest); err != nil {
			c.logf("KPI global shift: admit reference %s/%s: %v", business, e.Node, err)
		}
		c.mu.Lock()
		if b := c.biz[business]; b != nil {
			if b.ReferenceSince == nil {
				b.ReferenceSince = make(map[string]int64)
			}
			b.ReferenceSince[e.Node] = e.Since
			c.save()
		}
		c.mu.Unlock()
		c.logf("KPI global shift: %s/%s metric=%s ts=%v since=%d", business, e.Node, e.Metric, e.TsList, e.Since)
	}
}
