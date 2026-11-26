package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Computing-Availability-Tools/CATHelper/feature/straggler/config"
	"github.com/Computing-Availability-Tools/CATHelper/feature/straggler/profiling/dataparse"
	"github.com/Computing-Availability-Tools/CATHelper/feature/straggler/resource"
	"github.com/Computing-Availability-Tools/CATHelper/feature/straggler/utils"
)

// Daemon is the resident service: it runs one collect→convert→parse→analyse
// cycle per interval and exposes results + control over HTTP.
type Daemon struct {
	cfg    Config
	detect DetectFunc
	st     *store
	logf   func(format string, args ...any)

	mu                 sync.Mutex
	state              string            // "running" | "paused" | "managed"
	interval           time.Duration     // current cycle period (POST /daemon/interval updates it)
	iterations         int               // saved self-run iterations (POST /daemon/iterations updates it); center triggers carry their own value
	allowLowIterations bool              // --allow-low-iterations: permit iterations <3 at runtime (debug-only)
	thresholds         config.Thresholds // current detection thresholds (CLI initial, POST /daemon/thresholds updates them)
	nextRun            time.Time         // when the next cycle starts (zero when paused)
	cycleID            int               // per-process id, starting from 1
	cycleInFlight      bool
	cycleStartedAt     time.Time // start of the in-flight cycle (busy_eta_ms)
	pending            bool      // one queued next-cycle trigger
	pendingIterations  int
	pendingToken       string
	pendingDeadline    time.Time
	timer              *time.Timer   // cycle timer; stopped while paused, re-armed by Start/Trigger
	dynolog            *exec.Cmd     // dynolog child, left running on shutdown (nil = reusing existing)
	dynologPort        int           // dynolog's -port (passed to dyno as --port); 0 = unknown/not set
	stopOnce           sync.Once     // guards stopCh so POST /daemon/stop closes it exactly once
	stopCh             chan struct{} // closed by POST /daemon/stop to request graceful shutdown
	removeResults      bool          // set by Stop(): delete all daemon_results/ on shutdown

	progress *progressLog // live per-cycle stage log shown terminal-style in the console

	// managed (center-node) matching state — in-memory only, never persisted.
	// A daemon matches at most one center; the center re-generates the key per
	// match, so either side restarting forces a re-match.
	centerAddr string // matched center's base URL, e.g. "http://ip:port"
	matchKey   string // per-daemon secret, required on every controlled call
	business   string // business name this daemon reports under
	daemonID   string // daemon identifier within the business
	// centerInterval is the center business interval reported at match time.
	// On losing the center the daemon aligns its own self-trigger cadence to it
	// (so it neither bursts nor drifts while unmanaged).
	centerInterval time.Duration
	// reportEtas is the last 10 successful op_metric report latencies (ms).
	reportEtas []int64
}

// New creates a Daemon. detect is the shared profiler pipeline
// (main.detectFromParsedData); both daemon cycles and one-shot mode call it.
func New(cfg Config, detect DetectFunc) *Daemon {
	if cfg.Interval <= 0 {
		cfg.Interval = 10 * time.Minute
	}
	if cfg.CollectWait <= 0 {
		cfg.CollectWait = 60 * time.Second
	}
	if cfg.Port <= 0 {
		cfg.Port = 8080
	}
	if cfg.Iterations <= 0 {
		cfg.Iterations = 3
	}
	// Resolve --profiler-dir to an absolute path so the daemon's analyse/scan
	// (its own cwd) and the profiler writer (the training process / dynolog,
	// which resolves ACTIVITIES_LOG_FILE against ITS cwd) always refer to the
	// same directory. A relative path otherwise reads a different (empty) dir.
	if cfg.ProfilerDir != "" {
		if abs, err := filepath.Abs(cfg.ProfilerDir); err == nil {
			cfg.ProfilerDir = abs
		}
	}
	return &Daemon{
		cfg:                cfg,
		detect:             detect,
		st:                 newStore(),
		logf:               func(format string, args ...any) { fmt.Fprintf(os.Stderr, "[DAEMON] "+format+"\n", args...) },
		state:              "running",
		interval:           cfg.Interval,
		iterations:         cfg.Iterations,
		allowLowIterations: cfg.AllowLowIterations,
		thresholds:         cfg.Thresholds,
		stopCh:             make(chan struct{}),
		progress:           newProgressLog(),
	}
}

// Run starts the HTTP server and dynolog, then cycles on the interval ticker
// until ctx is cancelled (SIGINT/SIGTERM) or the HTTP server fails. The first
// cycle runs after the first interval tick, not at startup; use
// POST /daemon/trigger for an immediate cycle.
func (d *Daemon) Run(ctx context.Context) error {
	srv := d.httpServer()
	ln, err := net.Listen("tcp", fmt.Sprintf(":%d", d.cfg.Port))
	if err != nil {
		return fmt.Errorf("HTTP listen :%d: %w", d.cfg.Port, err)
	}
	srvErr := make(chan error, 1)
	go func() {
		if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			srvErr <- err
		}
	}()
	d.logf("HTTP server listening on :%d", d.cfg.Port)

	d.ensureDynolog()
	go d.kpiShipLoop(ctx)
	if d.cfg.KpiDir == "" {
		d.logf("KPI detection disabled (no --kpi-dir): cycles run profiler-only")
	} else {
		// Pre-flight: surface the KPI data source at startup so a wrong
		// --kpi-dir (or a CATMonitor straggler_output plugin that is off) is
		// visible immediately instead of silently skipping every cycle.
		n := countKPIFiles(d.cfg.KpiDir)
		d.logf("KPI detection enabled: --kpi-dir=%s (%d straggler_kpi_*.jsonl file(s))", d.cfg.KpiDir, n)
		if n == 0 {
			d.logf("WARNING: no straggler_kpi_*.jsonl found in %s — check that --kpi-dir points at CATMonitor's straggler_output.data_dir (default /var/lib/catmonitor/straggler) and that the straggler_output plugin is enabled", d.cfg.KpiDir)
		}
	}

	if d.cfg.DumpProfilerData {
		d.logf("dump profiler data enabled: keep newest %d copies under daemon_results/*/profiler_data", profilerDataKeep)
		d.cleanProfilerDataCopies()
		warnStaleProfilerDirs(d.cfg.ProfilerDir, d.logf)
	}

	// First cycle runs after the first interval tick, not immediately at
	// startup. nextRun is set so /status shows the scheduled first run; use
	// POST /daemon/trigger for an immediate cycle.
	d.mu.Lock()
	d.nextRun = time.Now().Add(d.interval)
	d.timer = time.NewTimer(d.interval)
	// The HTTP server starts before this point, so a Pause could already have
	// arrived; in that case the timer must not stay armed (Start() re-arms).
	if d.state == "paused" {
		d.nextRun = time.Time{}
		d.timer.Stop()
	}
	d.mu.Unlock()
	defer d.stopTimer()

	for {
		select {
		case <-ctx.Done():
			return d.shutdown(srv)
		case <-d.stopCh:
			return d.shutdown(srv)
		case err := <-srvErr:
			return err
		case <-d.timer.C:
			d.mu.Lock()
			if d.state == "running" {
				d.startCycle(d.iterations, "")
				// Re-anchor both the display and the actual timer to the next
				// interval. (startCycle already sets nextRun unless a cycle
				// was in flight and this tick was skipped.)
				d.nextRun = time.Now().Add(d.interval)
				d.timer.Reset(d.interval)
			} else {
				// Paused: stop the timer so it does not keep ticking through
				// the pause; Start() re-arms it from now+interval.
				d.stopTimer()
			}
			d.mu.Unlock()
		}
	}
}

// startCycle launches one cycle if none is in flight (single-flight: a long
// cycle skips the ticks that land while it runs) using iterations for THIS
// cycle. Caller holds d.mu.
func (d *Daemon) startCycle(iterations int, roundToken string) {
	if d.cycleInFlight {
		return
	}
	d.cycleInFlight = true
	d.cycleStartedAt = time.Now()
	d.cycleID++
	id := d.cycleID
	d.nextRun = time.Now().Add(d.interval)
	go func() {
		d.runCycle(id, iterations, roundToken)
		d.mu.Lock()
		d.cycleInFlight = false
		d.startPendingLocked()
		d.mu.Unlock()
	}()
}

// startPendingLocked launches the single queued trigger after the current cycle
// finishes. Caller holds d.mu.
func (d *Daemon) startPendingLocked() {
	if !d.pending {
		return
	}
	if d.state == "paused" {
		d.logf("drop pending cycle: daemon is paused (token=%q)", d.pendingToken)
		d.pending = false
		d.pendingIterations = 0
		d.pendingToken = ""
		d.pendingDeadline = time.Time{}
		return
	}
	if !d.pendingDeadline.IsZero() && time.Now().After(d.pendingDeadline) {
		d.logf("drop expired pending cycle (token=%q deadline=%s)", d.pendingToken, d.pendingDeadline.Format(time.RFC3339))
		d.pending = false
		d.pendingIterations = 0
		d.pendingToken = ""
		d.pendingDeadline = time.Time{}
		return
	}
	iterations, token := d.pendingIterations, d.pendingToken
	d.pending = false
	d.logf("start queued pending cycle: iterations=%d token=%q", iterations, token)
	d.startCycle(iterations, token)
}

// queuePending records at most one queued next-cycle trigger; a newer trigger
// overwrites the older one. The deadline bounds how late it may start.
func (d *Daemon) queuePending(iterations int, roundToken string, reportTimeoutSec int) {
	if reportTimeoutSec <= 0 {
		reportTimeoutSec = 480
	}
	if reportTimeoutSec < 120 {
		reportTimeoutSec = 120
	}
	if reportTimeoutSec > 1800 {
		reportTimeoutSec = 1800
	}
	d.mu.Lock()
	d.pending = true
	d.pendingIterations = iterations
	d.pendingToken = roundToken
	d.pendingDeadline = time.Now().Add(time.Duration(reportTimeoutSec) * time.Second)
	d.mu.Unlock()
	d.logf("cycle pending queued: iterations=%d token=%q timeout=%ds", iterations, roundToken, reportTimeoutSec)
}

// runCycle executes one collect→convert→parse→analyse pass and records the
// CycleResult (success or error) into the store.
func (d *Daemon) runCycle(id, iterations int, roundToken string) {
	cr := &CycleResult{ID: id, StartedAt: time.Now(), Iterations: iterations, RoundToken: roundToken}
	cr.DumpProfilerData = d.cfg.DumpProfilerData
	if d.cfg.DumpProfilerData {
		cr.ProfilerDataStatus = "pending"
	} else {
		cr.ProfilerDataStatus = "disabled"
	}
	d.progress.begin(id)
	d.progress.step("触发采集 (dyno)")
	// This cycle's analysed results (combined JSON, meta, report) are written
	// to ./daemon_results/<start>/ — OUTSIDE the --profiler-dir root — and
	// that archive dir is the cycle's dump_dir. The --profiler-dir root is only
	// the transient raw-collection input: dyno writes one
	// master_<pid>_<ts>_ascend_pt subdir PER RANK directly under it (the root,
	// not any single rank subdir, is what analyse/parse/detect operate on), and
	// cleanupDump deletes the whole root at the end of every cycle — so it must
	// not be reported as where this cycle's data lives.
	archive := filepath.Join("daemon_results", cr.StartedAt.Format("20060102-150405"))
	cr.DumpDir = archive
	defer func() {
		cr.FinishedAt = time.Now()
		cr.DurationMs = cr.FinishedAt.Sub(cr.StartedAt).Milliseconds()
		// Results are stored in daemon_results/<start>/ during the cycle; the
		// whole --profiler-dir is removed at the end of every cycle, success or
		// failure. dyno re-creates the root on the next trigger.
		if cr.Error != "" {
			d.progress.step("失败: %s", cr.Error)
		} else {
			d.progress.step("完成 (耗时 %dms, %d 个 .db)", cr.DurationMs, cr.DBs)
		}
		d.progress.finish()
		_ = d.progress.save(filepath.Join(archive, "progress.json"))
		// Log the cycle outcome BEFORE cleanup so the detected error (if any) is
		// printed ahead of the "cleaned" line — otherwise it looks like the dump
		// was removed before it was scanned.
		d.finishCycle(cr)
		d.cleanupDump(cr)
	}()

	// A raw dump left by a failed profiler_data copy must be moved aside before
	// dyno writes into the root. If it cannot be moved, fail without collecting.
	if err := d.prepareProfilerRoot(); err != nil {
		cr.keepRawDump = true
		cr.ProfilerDataStatus = "failed"
		cr.ProfilerDataError = fmt.Sprintf("stale_dump_present: %v", err)
		d.failCycle(cr, "collect", "stale_dump_present", err)
		return
	}

	// 1. Collect: dyno trigger -> verify commandStatus -> wait for the dump.
	if err := d.triggerCollection(cr.Iterations); err != nil {
		d.failCycle(cr, "collect", dynoFailureReason(err), err)
		return
	}
	d.progress.step("采集已触发，等待 %s ...", d.cfg.CollectWait)
	time.Sleep(d.cfg.CollectWait)
	root := d.cfg.ProfilerDir
	d.progress.step("采集完成")

	// Fail fast when the collector produced no raw dump at all: analyse on an
	// empty root costs 60-75s and cannot succeed.
	if !hasMasterDump(root) {
		d.failCycle(cr, "collect", "no_dump_after_collect",
			fmt.Errorf("no master_* dump under %s", root))
		return
	}

	// 2. Convert the raw dump to .db (torch_npu analyse) over the whole root.
	d.progress.step("转换 .db (torch_npu analyse)")
	if err := runAnalyse(root, d.logf); err != nil {
		d.failCycle(cr, "analyse", "no_db_after_analyse", err)
		return
	}

	// 3. Discover the .db files (recursive across all rank subdirs).
	dbFiles := findDBs(root)
	if len(dbFiles) == 0 {
		d.failCycle(cr, "analyse", "no_db_after_analyse", d.noDBsErr())
		return
	}
	cr.DBs = len(dbFiles)
	d.progress.step("发现 %d 个 .db 文件", len(dbFiles))

	// 4. Parse (StartProcess — not DataParsing, which os.Exit's on zero files).
	// Reset the per-path sync.Once dedup table: it is a package-level global
	// keyed by absolute path, and cleanupDump removed --profiler-dir last
	// cycle, so without a reset the stale Once objects make this cycle's
	// group_info_/host_info_ JSON writes no-ops → topology lost → slow
	// comm/CPU detection silently drops. One-shot mode is unaffected.
	dataparse.ResetFileWriteOnce()
	d.progress.step("解析 profiler 数据")
	if err := dataparse.StartProcess(dbFiles, root); err != nil {
		d.failCycle(cr, "parse", "parse_failed", err)
		return
	}
	d.progress.step("解析完成")

	// 4b. Global backfill pass: re-scan the .db files (still under root — the
	// dump is removed only at cycle end) and write per-domain bandwidth columns
	// into the CSVs so the bandwidth-based slow-communication detection (and
	// the op_metric archive/report below) sees them. Best-effort: a failure
	// only drops the bandwidth dimension, not the whole cycle.
	d.progress.step("回填慢域带宽列")
	if err := dataparse.BackfillSlowDomainBandwidth(root); err != nil {
		d.logf("cycle %d backfill slow-domain bandwidth: %v", cr.ID, err)
	}
	d.progress.step("回填完成")

	// 5. Stage B1: archive op_metric and report it to the center immediately.
	//    KPI/Profiler detection below must not delay this report.
	if err := d.archiveOpMetric(root, archive); err != nil {
		d.logf("cycle %d archive op_metric: %v", cr.ID, err)
	}
	d.progress.step("上报 op_metric 到中心")
	d.reportCycle(cr, "ok", "", "", root)

	// 6. Stage B2: KPI and Profiler detection are independent post-processing;
	//    run them in parallel after the report. config.Apply happens once before
	//    the goroutines; neither goroutine mutates package-level config.
	d.mu.Lock()
	th := d.thresholds
	d.mu.Unlock()
	config.Apply(th)

	var kpiRes *resource.DetectionResult
	var kpiStatus string
	var profRes *DetectResult
	var profErr error
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		d.progress.step("KPI 检测")
		kpiRes, kpiStatus = d.detectKPI()
		d.progress.step("KPI 检测完成 (%s)", kpiStatus)
	}()
	go func() {
		defer wg.Done()
		d.progress.step("Profiler 检测 (阈值 cal=%.2f cpu=%.2f)", th.Cal, th.CPU)
		profRes, profErr = d.detect(root, d.cfg.DebugOutput)
	}()
	wg.Wait()
	cr.KPI, cr.KPIStatus = kpiRes, kpiStatus

	if profErr != nil {
		// The center already received the op_metric; this only marks the local
		// history cycle as failed.
		cr.Error = fmt.Sprintf("profiler detection: %v", profErr)
		return
	}
	cr.Result = profRes.NodeOutput
	cr.Summary = profRes.Summary
	cr.CategoryStatus = profRes.CategoryStatus
	if profRes.AllNoData() && cr.KPI == nil {
		cr.Error = "no valid profiler data (no_data)"
		return
	}
	d.progress.step("Profiler 检测完成: cal=%d comm=%d cpu=%d",
		profRes.Summary["cal"], profRes.Summary["comm"], profRes.Summary["cpu"])
	// Merge the KPI anomaly counts (per metric) into the cycle summary so
	// history shows both dimensions; the kpi segment is absent when KPI
	// detection produced no result. Summary is a flat map: the profiler
	// categories (cal/comm/cpu) and each anomalous KPI metric's
	// count coexist as top-level keys.
	if cr.KPI != nil {
		mergeSummary(cr.Summary, kpiMetricCounts(cr.KPI, d.cfg.DebugOutput))
	}
	cr.Report = profRes.Report

	// 7. Write the combined result JSON + cycle meta into the per-cycle archive
	//    dir (cr.DumpDir = ./daemon_results/<start>/), OUTSIDE the raw dump
	//    root. Keeping results out of --profiler-dir is what lets the cycle's
	//    end delete the heavy profiler folder without touching the query data
	//    source.
	d.progress.step("写入结果到 %s", archive)
	if err := os.MkdirAll(archive, 0o755); err != nil {
		cr.Error = fmt.Sprintf("mkdir archive: %v", err)
		return
	}
	jsonPath := filepath.Join(archive, "straggler_output.json")
	if err := WriteCombinedJSON(cr.ID, cr.StartedAt.Format(time.RFC3339), cr.KPI, cr.Result, jsonPath); err != nil {
		cr.Error = fmt.Sprintf("write result JSON: %v", err)
		return
	}
	cr.JSONPath = jsonPath
	// daemon_meta.json is written by finishCycle before cleanup, then
	// overwritten with the profiler_data result after the copy.
	// Running-dir copy of the latest combined result (same shape as one-shot).
	if err := copyFile(jsonPath, filepath.Join(".", "straggler_output.json")); err != nil {
		d.logf("copy result to run dir: %v", err)
	}

	// 8. The text report is written inside the root (report.WriteReport derives
	//    its path from the input dir); copy it into the archive as a durable
	//    record. Best effort — cr.Report already holds the text in memory.
	if src := filepath.Join(root, "analysis_result", "detection_report.log"); fileExists(src) {
		if err := os.MkdirAll(filepath.Join(archive, "analysis_result"), 0o755); err != nil {
			d.logf("cycle %d copy report: %v", cr.ID, err)
		} else if err := copyFile(src, filepath.Join(archive, "analysis_result", "detection_report.log")); err != nil {
			d.logf("cycle %d copy report: %v", cr.ID, err)
		}
	}

	// op_metric/ was archived before the report (stage B1); no second copy.
}

// detectKPI reads the latest KPI data from --kpi-dir and runs the same
// resource detection as one-shot mode. It returns the result plus a status
// string surfaced in the cycle's history, so a disabled/skipped/failed KPI
// pass is visible instead of silently absent. The cycle itself continues
// (profiler still runs) when the directory is empty or detection fails.
func (d *Daemon) detectKPI() (*resource.DetectionResult, string) {
	if d.cfg.KpiDir == "" {
		return nil, "disabled (no --kpi-dir)"
	}
	ts, err := resource.ReadKPIFilesTail(d.cfg.KpiDir, resource.DefaultKPITailBytes)
	if err != nil {
		d.logf("KPI read skipped: %v", err)
		return nil, fmt.Sprintf("skipped: %v", err)
	}
	kpiCfg := resource.DefaultDetectionConfig()
	kpiCfg.EnableDebug = d.cfg.DebugOutput
	res, err := resource.RunDetectionFromData(ts, d.cfg.KpiDir, kpiCfg)
	if err != nil {
		d.logf("KPI detection failed: %v", err)
		return nil, fmt.Sprintf("failed: %v", err)
	}
	return res, "ok"
}

// mergeSummary copies src's key→count pairs into dst (dst wins on conflict).
func mergeSummary(dst, src map[string]int) {
	for k, v := range src {
		dst[k] = v
	}
}

// kpiMetricCounts builds the kpi sub-summary: per KPI metric, the number of
// anomalous cards (unit: 卡). Without --debug-output the result lists only
// anomalous cards; with debug every card is listed with an Abnormal flag, so
// only flagged cards are counted.
func kpiMetricCounts(kpi *resource.DetectionResult, debug bool) map[string]int {
	out := make(map[string]int)
	for _, ma := range kpi.Metrics {
		if !debug {
			out[string(ma.Metric)] = len(ma.Cards)
			continue
		}
		n := 0
		for _, c := range ma.Cards {
			if c.Abnormal {
				n++
			}
		}
		out[string(ma.Metric)] = n
	}
	return out
}

// countKPIFiles returns how many straggler_kpi_*.jsonl files ReadKPIFiles would
// consume for dir, mirroring its layout handling: with a node_config.json the
// files live inside the per-node subfolders it references; without one they are
// directly inside dir. Returns 0 when the dir is missing/empty or uses a
// different layout.
func countKPIFiles(dir string) int {
	// Optional node_config.json switches to the multi-node layout: folder →
	// {node, cards}; each folder holds that node's jsonl files.
	if raw, err := os.ReadFile(filepath.Join(dir, "node_config.json")); err == nil {
		var cfg map[string]struct {
			Node  string `json:"node"`
			Cards []int  `json:"cards"`
		}
		if json.Unmarshal(raw, &cfg) == nil && len(cfg) > 0 {
			n := 0
			for folder := range cfg {
				n += countKPIFilesIn(filepath.Join(dir, folder))
			}
			return n
		}
	}
	return countKPIFilesIn(dir)
}

func countKPIFilesIn(dir string) int {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	n := 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasPrefix(name, "straggler_kpi_") || !strings.HasSuffix(name, ".jsonl") {
			continue
		}
		n++
	}
	return n
}

// finishCycle records a finished cycle in the store and logs its outcome.
func (d *Daemon) finishCycle(cr *CycleResult) {
	d.st.add(cr)
	d.logf("cycle %d finished: dbs=%d error=%q", cr.ID, cr.DBs, cr.Error)
	if cr.DumpDir == "" {
		return
	}
	if err := os.MkdirAll(cr.DumpDir, 0o755); err != nil {
		d.logf("cycle %d mkdir archive for meta: %v", cr.ID, err)
		return
	}
	if err := writeMeta(cr.DumpDir, cr); err != nil {
		d.logf("cycle %d write daemon_meta.json: %v", cr.ID, err)
	}
}

// shutdown stops the HTTP server and waits for an in-flight cycle (max 10 min).
// The dynolog child is left running so collection can continue after exit.
func (d *Daemon) shutdown(srv *http.Server) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	_ = srv.Shutdown(ctx)

	deadline := time.Now().Add(10 * time.Minute)
	stillInFlight := false
	for {
		d.mu.Lock()
		inflight := d.cycleInFlight
		d.mu.Unlock()
		if !inflight {
			break
		}
		if time.Now().After(deadline) {
			d.logf("giving up waiting for in-flight cycle")
			stillInFlight = true
			break
		}
		time.Sleep(500 * time.Millisecond)
	}

	// The dynolog child is intentionally NOT killed here — it keeps running so
	// the user can still collect profiler data after the daemon exits.
	if d.removeResults {
		if stillInFlight {
			// A slow profiler_data copy can outlive the shutdown wait. Do not
			// delete the result tree underneath it.
			d.logf("not removing daemon_results: a cycle is still in flight")
		} else if err := os.RemoveAll("daemon_results"); err != nil {
			d.logf("remove daemon_results: %v", err)
		} else {
			d.logf("removed all daemon_results (dump_dir) files")
		}
	}
	d.logf("daemon stopped")
	return nil
}

// ---------------------------------------------------------------------------
// Control operations (called by the HTTP handlers; see server.go)
// ---------------------------------------------------------------------------

// Pause stops scheduling new cycles; an in-flight cycle finishes naturally.
// The cycle timer is stopped so the schedule does not keep ticking through
// the pause (Start() re-arms it from now+interval).
func (d *Daemon) Pause() {
	d.mu.Lock()
	defer d.mu.Unlock()
	// A queued center trigger must never start after a pause, even when the
	// daemon is managed (state stays "managed" while paused by the user).
	d.pending = false
	d.pendingIterations = 0
	d.pendingToken = ""
	d.pendingDeadline = time.Time{}
	if d.state == "running" {
		d.state = "paused"
		d.nextRun = time.Time{}
		d.stopTimer()
	}
}

// Start resumes the cycle loop and schedules the next run after the interval,
// re-arming the timer so the actual fire matches next_run_at.
func (d *Daemon) Start() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.state == "paused" {
		d.state = "running"
		d.nextRun = time.Now().Add(d.interval)
		d.resetTimer()
	}
}

// Thresholds returns the current detection thresholds.
func (d *Daemon) Thresholds() config.Thresholds {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.thresholds
}

// SetThresholds updates the detection thresholds for subsequent cycles
// (normalizing unset values). It does not touch already-completed results.
func (d *Daemon) SetThresholds(t config.Thresholds) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.thresholds = t.Normalized()
}

// SetInterval updates the cycle period, validating [60, 86400] seconds. The
// running timer is re-armed so the new period takes effect immediately.
func (d *Daemon) SetInterval(sec int64) error {
	min := d.minIntervalSec()
	if sec < min {
		return fmt.Errorf("interval_sec %d below minimum %d (report_eta + 130s)", sec, min)
	}
	if sec > 86400 {
		return fmt.Errorf("interval_sec out of range [%d, 86400]", min)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.interval = time.Duration(sec) * time.Second
	if d.state == "running" {
		d.nextRun = time.Now().Add(d.interval)
		d.resetTimer()
	}
	return nil
}

// Iterations returns the saved self-run iteration count (used by the daemon's
// own timer/manual trigger when it is not managed by a center).
func (d *Daemon) Iterations() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.iterations
}

// validateIterations enforces the iteration range and the low-iteration
// debug guardrail shared by the saved value and per-trigger overrides.
func (d *Daemon) validateIterations(n int) error {
	if n < 1 || n > 100 {
		return fmt.Errorf("iterations out of range [1, 100]")
	}
	if n < 3 && !d.allowLowIterations {
		return fmt.Errorf("iterations below 3 require --allow-low-iterations at daemon startup")
	}
	return nil
}

// SetIterations updates the daemon's saved self-run iteration count, validating
// [1, 100]. Values below 3 are only accepted when the daemon was started with
// --allow-low-iterations. This value is used by the daemon's own timer/manual
// trigger when it is not managed by a center; a center-triggered cycle uses the
// value carried by the trigger request instead.
func (d *Daemon) SetIterations(n int) error {
	if err := d.validateIterations(n); err != nil {
		return err
	}
	d.mu.Lock()
	d.iterations = n
	d.mu.Unlock()
	if n < 3 {
		d.logf("WARNING: iterations=%d (<3) may sample too few steps and make a round look normal", n)
	}
	return nil
}

// errCycleInFlight is returned by TriggerWithOptions when another cycle is
// running. The HTTP handler reports status=busy for this case.
var errCycleInFlight = errors.New("a cycle is already running")

// Trigger runs one cycle immediately with the daemon's saved iterations;
// returns an error when paused or when a cycle is already in flight (HTTP 409).
// In "running" the timer is re-anchored so the next automatic cycle is one
// interval after this manual one; in "managed" the timer stays stopped (the
// center schedules the cycles).
func (d *Daemon) Trigger() error {
	return d.TriggerWithOptions(d.Iterations(), "")
}

// TriggerWithOptions runs one cycle with an explicit per-cycle iteration count.
// The value is used by THIS cycle only and does not modify the daemon's saved
// self-run value. roundToken is carried through the cycle and echoed back with
// the op_metric report so the center can reject a late report from an older
// round.
func (d *Daemon) TriggerWithOptions(iterations int, roundToken string) error {
	if err := d.validateIterations(iterations); err != nil {
		return err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.state == "paused" {
		return fmt.Errorf("daemon is paused")
	}
	if d.cycleInFlight {
		return errCycleInFlight
	}
	d.startCycle(iterations, roundToken)
	if d.state == "running" {
		d.resetTimer()
	}
	return nil
}

// Stop requests a graceful shutdown of the daemon: Run's select observes the
// closed stopCh and runs shutdown (HTTP server close, in-flight cycle wait).
// The dynolog child is left running. Idempotent — repeated calls are no-ops.
// All archived result files under daemon_results/ (each cycle's dump_dir) are
// removed as part of the shutdown.
func (d *Daemon) Stop() {
	d.stopOnce.Do(func() {
		d.mu.Lock()
		d.removeResults = true
		d.mu.Unlock()
		close(d.stopCh)
	})
}

// ---------------------------------------------------------------------------
// Managed (center-node) matching — in-memory only, key per match
// ---------------------------------------------------------------------------

// Match establishes a managed relationship with a center. The daemon stops
// self-scheduling (state=managed), records the center/key/business/daemon and
// starts the reverse heartbeat. Returns an error if already managed.
func (d *Daemon) Match(centerAddr, key, business, daemonID string, interval time.Duration) error {
	if interval > 0 {
		if min := d.minIntervalSec(); int64(interval.Seconds()) < min {
			return fmt.Errorf("interval_sec %d below minimum %d (report_eta + 130s)", int64(interval.Seconds()), min)
		}
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.state == "managed" {
		if d.centerAddr != centerAddr {
			return fmt.Errorf("already managed by a center")
		}
		// Same center re-key: the center restarted or the business was
		// recreated, so accept its new key without restarting heartbeat.
		d.matchKey = key
		d.business = business
		d.daemonID = daemonID
		if interval > 0 {
			d.centerInterval = interval
		}
		d.logf("re-matched by center %s (business=%s daemon=%s)", centerAddr, business, daemonID)
		return nil
	}
	d.centerAddr = centerAddr
	d.matchKey = key
	d.business = business
	d.daemonID = daemonID
	if interval > 0 {
		d.centerInterval = interval
	}
	d.state = "managed"
	d.nextRun = time.Time{}
	d.stopTimer()
	go d.heartbeatLoop()
	d.logf("matched by center %s (business=%s daemon=%s)", centerAddr, business, daemonID)
	return nil
}

// Unmatch ends the managed relationship and returns to running (self-scheduling
// resumes). Idempotent when not managed.
func (d *Daemon) Unmatch() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.state != "managed" {
		return
	}
	d.centerAddr = ""
	d.matchKey = ""
	d.business = ""
	d.daemonID = ""
	d.state = "running"
	d.pending = false
	// Align the self-trigger cadence with the center's business interval and
	// schedule the first self-trigger one FULL interval out: losing the center
	// must not replay the rounds it missed (no catch-up burst).
	if d.centerInterval > 0 {
		d.interval = d.centerInterval
	}
	if min := time.Duration(d.minIntervalSecLocked()) * time.Second; d.interval < min {
		d.interval = min
	}
	d.nextRun = time.Now().Add(d.interval)
	d.resetTimer()
	d.logf("unmatched, back to running (interval=%s)", d.interval)
}

// IsManaged reports whether the daemon is currently managed by a center.
func (d *Daemon) IsManaged() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.state == "managed"
}

// KeyMatches reports whether key equals the current (non-empty) match key.
func (d *Daemon) KeyMatches(key string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.state == "managed" && key != "" && d.matchKey == key
}

// matchState returns the daemon's state plus match info for /daemon/match_status.
func (d *Daemon) matchState() (state, center string, matched bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.state, d.centerAddr, d.state == "managed" && d.matchKey != ""
}

// heartbeatLoop polls the center's /center/healthz while managed; 3 consecutive
// failures (center down) unmatch the daemon back to running.
func (d *Daemon) heartbeatLoop() {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	fails := 0
	for range ticker.C {
		d.mu.Lock()
		if d.state != "managed" {
			d.mu.Unlock()
			return
		}
		center := d.centerAddr
		d.mu.Unlock()

		if centerAlive(center) {
			fails = 0
			continue
		}
		fails++
		if fails >= 3 {
			d.logf("center %s heartbeat lost — unmatching", center)
			d.Unmatch()
			return
		}
	}
}

// reportCycle sends this cycle's op_metric report to the matched center. status
// is ok (ranks filled from the archived op_metric), or failed with reason/stage.
// Best-effort: the center's timeout is the final fallback. Self-run cycles are
// not reported (ReportStatus=skipped).
func (d *Daemon) reportCycle(cr *CycleResult, status, reason, stage, fallbackRoot string) {
	cr.ReportStatus = status
	cr.ReportReason = reason
	cr.ReportStage = stage
	if cr.RoundToken == "" || !d.IsManaged() {
		cr.ReportStatus = "skipped"
		return
	}
	var ranks any
	if status == "ok" {
		view, err := buildOpMetricView(cr)
		if err != nil && fallbackRoot != "" {
			view, err = buildOpMetricViewAt(filepath.Join(fallbackRoot, "op_metric"), cr.ID)
		}
		if err != nil || len(view.Ranks) == 0 {
			if err != nil {
				d.logf("cycle %d op_metric view: %v", cr.ID, err)
			} else {
				d.logf("cycle %d op_metric view is empty", cr.ID)
			}
			status, reason, stage = "failed", "report_build_failed", "backfill"
			cr.ReportStatus, cr.ReportReason, cr.ReportStage = status, reason, stage
		} else {
			ranks = view.Ranks
		}
	}
	start := time.Now()
	if err := d.postReport(cr.RoundToken, status, reason, stage, ranks, nil); err != nil {
		cr.ReportStatus = "undelivered"
		d.logf("cycle %d report (%s/%s): %v", cr.ID, status, stage, err)
		return
	}
	if status == "ok" {
		cr.ReportEtaMs = time.Since(cr.StartedAt).Milliseconds()
		d.recordReportEta(cr.ReportEtaMs)
		d.logf("cycle %d report ok: eta=%dms http=%dms", cr.ID, cr.ReportEtaMs, time.Since(start).Milliseconds())
	}
}

// postReport posts one report (5s timeout, one retry). It is a no-op when the
// daemon is not managed or has no round token (self-run).
func (d *Daemon) postReport(roundToken, status, reason, stage string, ranks any, extra map[string]any) error {
	d.mu.Lock()
	if d.state != "managed" || d.centerAddr == "" || roundToken == "" {
		d.mu.Unlock()
		return nil
	}
	center := d.centerAddr
	key := d.matchKey
	business := d.business
	daemonID := d.daemonID
	d.mu.Unlock()

	payload := map[string]any{
		"round_token": roundToken,
		"status":      status,
		"ranks":       ranks,
	}
	if reason != "" {
		payload["reason"] = reason
	}
	if stage != "" {
		payload["stage"] = stage
	}
	for k, v := range extra {
		payload[k] = v
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	target := strings.TrimRight(center, "/") + "/center/op_metric/" + url.PathEscape(business) + "/" + url.PathEscape(daemonID)
	timeout := 5 * time.Second
	if status == "ok" {
		// The ok payload carries the full rank and artifact maps; keep the old
		// 10s budget for it. Failure/busy/paused payloads stay at 5s.
		timeout = 10 * time.Second
	}
	client := &http.Client{Timeout: timeout}
	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		req, rerr := http.NewRequest(http.MethodPost, target, bytes.NewReader(body))
		if rerr != nil {
			return rerr
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Match-Key", key)
		resp, rerr := client.Do(req)
		if rerr == nil {
			rb, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
			lastErr = fmt.Errorf("center returned %d: %s", resp.StatusCode, strings.TrimSpace(string(rb)))
		} else {
			lastErr = rerr
		}
	}
	return lastErr
}

func (d *Daemon) recordReportEta(ms int64) {
	if ms <= 0 {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.reportEtas = append(d.reportEtas, ms)
	if len(d.reportEtas) > 10 {
		d.reportEtas = d.reportEtas[len(d.reportEtas)-10:]
	}
}

// reportEtaP90 returns the 90th-percentile successful report latency (ms) over
// the last 10 successful cycles; with fewer than 3 samples it returns the max.
func (d *Daemon) reportEtaP90() int64 {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.reportEtaP90Locked()
}

func (d *Daemon) reportEtaP90Locked() int64 {
	vals := append([]int64(nil), d.reportEtas...)
	n := len(vals)
	if n == 0 {
		return 0
	}
	if n < 3 {
		max := vals[0]
		for _, v := range vals {
			if v > max {
				max = v
			}
		}
		return max
	}
	sort.Slice(vals, func(i, j int) bool { return vals[i] < vals[j] })
	idx := int(math.Ceil(0.9*float64(n))) - 1
	if idx < 0 {
		idx = 0
	}
	if idx >= n {
		idx = n - 1
	}
	return vals[idx]
}

// minIntervalSec is the minimum safe self-run interval: 300s, or
// reportEta + 130s when a report ETA is known.
func (d *Daemon) minIntervalSec() int64 {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.minIntervalSecLocked()
}

func (d *Daemon) minIntervalSecLocked() int64 {
	min := int64(300)
	if eta := d.reportEtaP90Locked(); eta > 0 {
		if v := eta/1000 + 130; v > min {
			min = v
		}
	}
	return min
}

// daemonPostProcessEtaMs is the fallback post-processing allowance used when
// estimating how much of the in-flight cycle remains.
const daemonPostProcessEtaMs = 70_000

// busyEtaMsLocked estimates how long the in-flight cycle still needs, including
// a fixed post-processing allowance. Caller holds d.mu.
func (d *Daemon) busyEtaMsLocked() int64 {
	if !d.cycleInFlight || d.cycleStartedAt.IsZero() {
		return 0
	}
	eta := d.reportEtaP90Locked()
	if eta <= 0 {
		return 0
	}
	remaining := eta + daemonPostProcessEtaMs - time.Since(d.cycleStartedAt).Milliseconds()
	if remaining < 0 {
		return 0
	}
	return remaining
}

// failCycle records the local failure and reports it to the center immediately,
// then returns (the caller skips all later stages).
func (d *Daemon) failCycle(cr *CycleResult, stage, reason string, cause error) {
	if cause != nil {
		cr.Error = fmt.Sprintf("%s: %v", reason, cause)
	} else {
		cr.Error = reason
	}
	d.progress.step("上报失败到中心 (%s/%s)", stage, reason)
	d.reportCycle(cr, "failed", reason, stage, "")
}

func dynoFailureReason(err error) string {
	if err != nil && strings.Contains(err.Error(), "processesMatched empty") {
		return "no_target_process"
	}
	return "dyno_trigger_failed"
}

func hasMasterDump(root string) bool {
	entries, err := os.ReadDir(root)
	if err != nil {
		return false
	}
	for _, e := range entries {
		if e.IsDir() && strings.HasPrefix(e.Name(), "master_") {
			return true
		}
	}
	return false
}

// archiveOpMetric copies <root>/op_metric into the per-cycle archive before the
// report, so the report is not delayed by KPI/Profiler post-processing.
func (d *Daemon) archiveOpMetric(root, archive string) error {
	src := filepath.Join(root, "op_metric")
	if !fileExistsDir(src) {
		return fmt.Errorf("op_metric missing under %s", root)
	}
	if err := os.MkdirAll(archive, 0o755); err != nil {
		return err
	}
	return copyDir(src, archive)
}

// centerAlive checks a center's /center/healthz endpoint.
func centerAlive(center string) bool {
	if center == "" {
		return false
	}
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get(strings.TrimRight(center, "/") + "/center/healthz")
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64))
	return resp.StatusCode == http.StatusOK && strings.TrimSpace(string(body)) == "ok"
}

// stopTimer stops the cycle timer, draining any stale fire so a later Reset
// takes effect cleanly (Stop returning false means a value may be pending in
// C). Caller holds d.mu.
func (d *Daemon) stopTimer() {
	if d.timer == nil {
		return
	}
	if !d.timer.Stop() {
		select {
		case <-d.timer.C:
		default:
		}
	}
}

// resetTimer re-arms the cycle timer to fire after the current interval.
// Caller holds d.mu.
func (d *Daemon) resetTimer() {
	d.stopTimer()
	if d.timer != nil {
		d.timer.Reset(d.interval)
	}
}

// ---------------------------------------------------------------------------
// Result JSON helpers
// ---------------------------------------------------------------------------

// WriteCombinedJSON marshals the KPI + profiler result into one JSON file at
// path — the shared straggler_output.json shape
// ({"id": ..., "started_at": ..., "kpi": ..., "profiler": ...}). id/startedAt
// are the cycle id and start time (RFC3339) in daemon mode; pass 0/"" in
// one-shot mode to omit them.
func WriteCombinedJSON(id int, startedAt string, kpi *resource.DetectionResult, profiler *utils.NodeOutput, path string) error {
	out := CombinedOutput{ID: id, StartedAt: startedAt, KPI: kpi, Profiler: profiler}
	data, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal combined output: %w", err)
	}
	if err := os.WriteFile(path, data, 0644); err != nil {
		return fmt.Errorf("write combined output: %w", err)
	}
	return nil
}

// writeMeta serializes the lightweight cycle metadata into the dump directory.
func writeMeta(dumpDir string, cr *CycleResult) error {
	data, err := json.MarshalIndent(cr, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dumpDir, "daemon_meta.json"), data, 0644)
}

func copyFile(src, dst string) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, data, 0644)
}

// copyDir recursively copies src into dst (dst/src's basename is created).
// Errors are collected per-file; a non-nil error is returned only when the
// top-level src cannot be walked — individual file failures do not abort the
// copy so a partial op_metric still survives.
func copyDir(src, dst string) error {
	info, err := os.Stat(src)
	if err != nil {
		return err
	}
	target := filepath.Join(dst, info.Name())
	if err := os.MkdirAll(target, 0o755); err != nil {
		return err
	}
	return filepath.Walk(src, func(path string, fi os.FileInfo, werr error) error {
		if werr != nil {
			return werr
		}
		rel, rerr := filepath.Rel(src, path)
		if rerr != nil {
			return rerr
		}
		out := filepath.Join(target, rel)
		if fi.IsDir() {
			return os.MkdirAll(out, fi.Mode())
		}
		return copyFile(path, out)
	})
}

// cleanupDump removes the entire --profiler-dir at the end of every cycle,
// success or failure. dyno re-creates the root on the next trigger, so nothing
// is lost and no stale dump can skew a later cycle's newest-dir pick. Result
// artifacts live in daemon_results/<start>/ (outside profiler-dir) and are
// untouched; RemoveAll on a missing root is a no-op, so this is safe even when
// the trigger never created a dump. Failures are logged, never fatal.
func (d *Daemon) cleanupDump(cr *CycleResult) {
	res := d.applyProfilerDataPolicy(cr)
	d.st.setProfilerData(cr.ID, res)
	if cr.DumpDir != "" {
		if meta := d.st.get(cr.ID); meta != nil {
			if err := os.MkdirAll(cr.DumpDir, 0o755); err == nil {
				if err := writeMeta(cr.DumpDir, meta); err != nil {
					d.logf("cycle %d update daemon_meta.json: %v", cr.ID, err)
				}
			}
		}
	}
	// A failed copy (or a stale-dump rename failure) keeps the raw root as
	// evidence; the next cycle moves it aside before dyno runs.
	if res.KeepDump {
		d.logf("cycle %d kept raw profiler dump for diagnosis: %s", cr.ID, d.cfg.ProfilerDir)
		return
	}
	if _, err := os.Stat(d.cfg.ProfilerDir); err != nil {
		return // nothing was written this cycle; nothing to clean
	}
	if err := os.RemoveAll(d.cfg.ProfilerDir); err != nil {
		d.logf("cycle %d cleanup failed: %v", cr.ID, err)
		return
	}
	d.logf("cycle %d cleaned: removed %s", cr.ID, d.cfg.ProfilerDir)
}
