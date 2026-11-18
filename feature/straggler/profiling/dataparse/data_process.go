package dataparse

import (
	"database/sql"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// ---------------------------------------------------------------------------
// Concurrency control
// ---------------------------------------------------------------------------

var (
	csvMutex sync.Mutex

	fileWriteOnce   = make(map[string]*sync.Once)
	fileWriteOnceMu sync.Mutex
)

// ResetFileWriteOnce clears the dedup table that guards group_info_/host_info_
// JSON writes. The table is a package-level global keyed by absolute file path,
// so a long-lived daemon that deletes and re-creates --profiler-dir each cycle
// would otherwise keep stale *sync.Once objects for the same paths: the second
// and later cycles' once.Do become no-ops, the JSON files stop being written,
// GetCurDetectionInfo falls back to CSV-only rank collection (no parallel
// topology), and slow-communication/CPU detection silently lose input.
// One-shot mode is unaffected (single pass, first Do always fires).
// Call this at the start of every daemon cycle, before StartProcess.
func ResetFileWriteOnce() {
	fileWriteOnceMu.Lock()
	fileWriteOnce = make(map[string]*sync.Once)
	fileWriteOnceMu.Unlock()
}

// ---------------------------------------------------------------------------
// ProcessDatabase — per-file pipeline
// ---------------------------------------------------------------------------

// ProcessDatabase opens an SQLite database, extracts profiling data, computes
// metrics, and writes a single-line CSV to op_metric/.
func ProcessDatabase(dbFilePath string, outputDir string) error {
	db, err := sql.Open("sqlite", dbFilePath+"?mode=ro")
	if err != nil {
		return fmt.Errorf("open db %s: %w", dbFilePath, err)
	}
	defer db.Close()

	// WAL mode
	db.Exec("PRAGMA journal_mode=WAL")

	// Create indices (idempotent).
	for _, idx := range []string{
		"CREATE INDEX IF NOT EXISTS idx_string_ids_value ON STRING_IDS(value)",
		"CREATE INDEX IF NOT EXISTS idx_device_op_time ON COMMUNICATION_OP(startNs, endNs)",
		"CREATE INDEX IF NOT EXISTS idx_task_time_type ON TASK(startNs, endNs, taskType)",
	} {
		if _, err := db.Exec(idx); err != nil {
			// Index creation failures are non-fatal (table may not exist yet).
			_ = err
		}
	}

	// Extract global rank from filename.
	rankStr, err := extractGlobalRankFromFilename(dbFilePath)
	if err != nil {
		return err
	}

	// Query HOST_INFO for hostUid (physical node id) and hostName (node name).
	hostUid, hostName, _ := queryHostInfo(db)
	// Query NPU_INFO for this device's NPU id.
	npuID, _ := queryNpuID(db)

	// Read parallel group info from META_DATA.
	groupInfo, xpToGroupName, err := readGroupInfo(db, rankStr, outputDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[DATA PROCESS] WARNING: %v\n", err)
	}

	// Get all step time ranges.
	steps, err := GetAllStepTimes(db)
	if err != nil || len(steps) == 0 {
		return fmt.Errorf("no step data in %s", dbFilePath)
	}

	// Merge all steps into one.
	minStart := math.MaxInt
	maxEnd := math.MinInt
	for _, s := range steps {
		if s.StartNs < minStart {
			minStart = s.StartNs
		}
		if s.EndNs > maxEnd {
			maxEnd = s.EndNs
		}
	}
	mergedStep := StepTime{ID: 0, StartNs: minStart, EndNs: maxEnd}

	// Compute metrics.
	metrics, err := TimeDiffForStep(db, xpToGroupName, mergedStep)
	if err != nil {
		return fmt.Errorf("metric calculation for rank %s: %w", rankStr, err)
	}
	metrics.StepIndex = mergedStep.ID
	metrics.StepDuration = mergedStep.EndNs - mergedStep.StartNs
	metrics.HostUid = hostUid

	// Write CSV.
	csvPath := filepath.Join(outputDir, "op_metric", "global_rank_"+rankStr+".csv")
	if err := WriteResultsToCSV(csvPath, []PerformanceMetrics{metrics}); err != nil {
		return fmt.Errorf("write CSV %s: %w", csvPath, err)
	}

	// Write host_info_{N}.json (rank → hostUid/hostName for slow-CPU + node output).
	writeHostInfo(outputDir, rankStr, hostUid, hostName)
	// Write npu_info_{N}.json (rank → NPU id for node-based output).
	writeNpuInfo(outputDir, rankStr, npuID)

	_ = groupInfo
	return nil
}

// ---------------------------------------------------------------------------
// readGroupInfo
// ---------------------------------------------------------------------------

func readGroupInfo(db *sql.DB, rankStr, outputDir string) (map[string]interface{}, map[string]string, error) {
	row := db.QueryRow("SELECT value FROM META_DATA WHERE name = 'parallel_group_info'")
	var raw string
	if err := row.Scan(&raw); err != nil {
		return nil, nil, fmt.Errorf("parallel_group_info not found for rank %s: %w", rankStr, err)
	}

	var data map[string]interface{}
	if err := json.Unmarshal([]byte(raw), &data); err != nil {
		return nil, nil, fmt.Errorf("parse parallel_group_info JSON: %w", err)
	}

	// Write group_info_{N}.json exactly once per unique filename.
	outDir := filepath.Join(outputDir, "op_metric")
	os.MkdirAll(outDir, 0755)
	jsonPath := filepath.Join(outDir, "group_info_"+rankStr+".json")

	fileWriteOnceMu.Lock()
	if fileWriteOnce[jsonPath] == nil {
		fileWriteOnce[jsonPath] = &sync.Once{}
	}
	once := fileWriteOnce[jsonPath]
	fileWriteOnceMu.Unlock()

	once.Do(func() {
		pretty, _ := json.MarshalIndent(data, "", "  ")
		os.WriteFile(jsonPath, pretty, 0644)
	})

	// Build xpToGroupName: smallerIndexName → original key (e.g. "tp" → "group_name_42")
	xpToGroupName := make(map[string]string)
	for key, val := range data {
		if m, ok := val.(map[string]interface{}); ok {
			if gn, ok := m["group_name"].(string); ok && gn != "" {
				xpToGroupName[gn] = key
			}
		}
	}
	return data, xpToGroupName, nil
}

// queryHostInfo reads the hostUid and hostName columns from the HOST_INFO
// table. There is theoretically only one record per device database. Returns
// empty strings if the table does not exist or the query fails.
func queryHostInfo(db *sql.DB) (hostUid, hostName string, err error) {
	exists, _ := tableExists(db, "HOST_INFO")
	if !exists {
		return "", "", fmt.Errorf("HOST_INFO table not found")
	}
	err = db.QueryRow("SELECT hostUid, hostName FROM HOST_INFO LIMIT 1").Scan(&hostUid, &hostName)
	if err != nil {
		return "", "", err
	}
	return hostUid, hostName, nil
}

// queryNpuID reads the id column from the NPU_INFO table (one record per
// device database). Returns 0 if the table does not exist or the query fails.
func queryNpuID(db *sql.DB) (int, error) {
	exists, _ := tableExists(db, "NPU_INFO")
	if !exists {
		return 0, fmt.Errorf("NPU_INFO table not found")
	}
	var id int
	err := db.QueryRow("SELECT id FROM NPU_INFO LIMIT 1").Scan(&id)
	if err != nil {
		return 0, err
	}
	return id, nil
}

// writeHostInfo writes a per-rank JSON file containing the hostUid and
// hostName for slow-CPU grouping and node-based output. Uses sync.Once to
// avoid duplicate writes.
func writeHostInfo(outputDir, rankStr, hostUid, hostName string) {
	outDir := filepath.Join(outputDir, "op_metric")
	os.MkdirAll(outDir, 0755)
	jsonPath := filepath.Join(outDir, "host_info_"+rankStr+".json")

	fileWriteOnceMu.Lock()
	if fileWriteOnce[jsonPath] == nil {
		fileWriteOnce[jsonPath] = &sync.Once{}
	}
	once := fileWriteOnce[jsonPath]
	fileWriteOnceMu.Unlock()

	once.Do(func() {
		payload := map[string]string{"rank": rankStr, "hostUid": hostUid, "hostName": hostName}
		pretty, _ := json.MarshalIndent(payload, "", "  ")
		os.WriteFile(jsonPath, pretty, 0644)
	})
}

// writeNpuInfo writes a per-rank JSON file containing the NPU id for node-based
// output. Uses sync.Once to avoid duplicate writes.
func writeNpuInfo(outputDir, rankStr string, npuID int) {
	outDir := filepath.Join(outputDir, "op_metric")
	os.MkdirAll(outDir, 0755)
	jsonPath := filepath.Join(outDir, "npu_info_"+rankStr+".json")

	fileWriteOnceMu.Lock()
	if fileWriteOnce[jsonPath] == nil {
		fileWriteOnce[jsonPath] = &sync.Once{}
	}
	once := fileWriteOnce[jsonPath]
	fileWriteOnceMu.Unlock()

	once.Do(func() {
		payload := map[string]interface{}{"rank": rankStr, "id": npuID}
		pretty, _ := json.MarshalIndent(payload, "", "  ")
		os.WriteFile(jsonPath, pretty, 0644)
	})
}

// ---------------------------------------------------------------------------
// GetAllStepTimes — 3-level fallback
// ---------------------------------------------------------------------------

// GetAllStepTimes tries STEP_TIME first, then TASK+STRING_IDS+MSTX_EVENTS,
// finally a sentinel step spanning the entire time range.
func GetAllStepTimes(db *sql.DB) ([]StepTime, error) {
	exists, _ := tableExists(db, "STEP_TIME")
	if exists {
		steps, err := GetStepTimesFromSTEP_TIME(db)
		if err == nil && len(steps) > 0 {
			return steps, nil
		}
	}

	exists, _ = tableExists(db, "TASK")
	if exists {
		steps, err := GetStepTimesFromTASK(db)
		if err == nil && len(steps) > 0 {
			return steps, nil
		}
	}

	// Sentinel fallback.
	return []StepTime{{ID: -1, StartNs: math.MinInt, EndNs: math.MaxInt}}, nil
}

// GetStepTimesFromSTEP_TIME reads all step entries in descending ID order
// and reverses them to ascending.
func GetStepTimesFromSTEP_TIME(db *sql.DB) ([]StepTime, error) {
	rows, err := db.Query("SELECT id, startNs, endNs FROM STEP_TIME ORDER BY id DESC")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var steps []StepTime
	for rows.Next() {
		var s StepTime
		if err := rows.Scan(&s.ID, &s.StartNs, &s.EndNs); err != nil {
			return nil, err
		}
		steps = append(steps, s)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Reverse to ascending order.
	for i, j := 0, len(steps)-1; i < j; i, j = i+1, j-1 {
		steps[i], steps[j] = steps[j], steps[i]
	}
	return steps, nil
}

var stepPattern = regexp.MustCompile(`^step\s+\d+$`)

// GetStepTimesFromTASK extracts step boundaries from TASK + STRING_IDS + MSTX_EVENTS.
func GetStepTimesFromTASK(db *sql.DB) ([]StepTime, error) {
	// Find all STRING_IDS entries matching "step N".
	sRows, err := db.Query("SELECT id, value FROM STRING_IDS")
	if err != nil {
		return nil, err
	}
	defer sRows.Close()

	type stepMsg struct{ sid, connID int }
	var msgs []stepMsg
	for sRows.Next() {
		var id int
		var val string
		if err := sRows.Scan(&id, &val); err != nil {
			continue
		}
		if !stepPattern.MatchString(strings.ToLower(val)) {
			continue
		}
		// Look up in MSTX_EVENTS.
		mRow := db.QueryRow("SELECT connectionId FROM MSTX_EVENTS WHERE message = ?", id)
		var cid int
		if err := mRow.Scan(&cid); err != nil {
			continue
		}
		msgs = append(msgs, stepMsg{sid: id, connID: cid})
	}
	if len(msgs) == 0 {
		return nil, fmt.Errorf("no step markers found in TASK fallback")
	}

	var steps []StepTime
	for i, m := range msgs {
		tRow := db.QueryRow("SELECT startNs, endNs FROM TASK WHERE connectionId = ?", m.connID)
		var s StepTime
		s.ID = i
		if err := tRow.Scan(&s.StartNs, &s.EndNs); err != nil {
			continue
		}
		steps = append(steps, s)
	}
	if len(steps) == 0 {
		return nil, fmt.Errorf("no step times from TASK")
	}
	return steps, nil
}

// ---------------------------------------------------------------------------
// TimeDiffForStep — main metric computation
// ---------------------------------------------------------------------------

// TimeDiffForStep extracts all performance metrics for a single (merged) step
// from the database.
func TimeDiffForStep(db *sql.DB, xpToGroupName map[string]string, stepTime StepTime) (PerformanceMetrics, error) {
	pm := PerformanceMetrics{
		Durations: make(map[string]int),
		Counts:    make(map[string]int),
	}

	// 1. Query DataLoader ID.
	dlID, _ := queryDataLoaderID(db)

	// 2. Query KERNEL_AICORE connection IDs for host-time fallback.
	kernelConnIDs, _ := getKernelAicoreTaskConnectionIDs(db, stepTime)

	// No parallel group info → Python returns an all-zero snapshot.
	if len(xpToGroupName) == 0 {
		return pm, nil
	}

	// 3. Build groupName → string_id mapping.
	groupNames := make([]string, 0, len(xpToGroupName))
	for _, origKey := range xpToGroupName {
		groupNames = append(groupNames, origKey)
	}

	groupNameIDMap, err := batchQueryStringIDs(db, groupNames)
	if err != nil {
		return pm, err
	}
	if len(groupNameIDMap) == 0 {
		if durs := collectKernelHostDurations(db, kernelConnIDs); len(durs) > 0 {
			pm.ZPHost, _ = CalculateMean(durs)
		} else {
			pm.ZPHost = 0
		}
		pm.ZPDuration = invalidData
		pm.ZPDevice = invalidData
		pm.ZPKernel, _ = GetAvgKernelTaskDuration(db, stepTime)
		pm.DataLoader, _ = queryDataLoaderDuration(db, dlID, stepTime)
		return pm, nil
	}

	// Build reverse: STRING_IDS id → short group name (e.g. the STRING_IDS id of
	// "group_name_42" maps to the short name "tp"). xpToGroupName is keyed by
	// the short name ("tp") and valued by the group name found in STRING_IDS
	// ("group_name_42"), so iterate it the other way around — the old code
	// looked up the long name in the short-name keyed map and always missed,
	// leaving idToXp empty and the CSV without any {domain}_Duration column.
	idToXp := make(map[int]string)
	for shortName, longName := range xpToGroupName {
		if sid, ok := groupNameIDMap[longName]; ok {
			idToXp[sid] = shortName
		}
	}

	// Build group name id list for SQL IN clause.
	groupNameIDs := make([]int, 0, len(groupNameIDMap))
	for _, sid := range groupNameIDMap {
		groupNameIDs = append(groupNameIDs, sid)
	}

	// 4. Query communication operators.
	devOps, err := getDeviceOpList(db, groupNameIDs, stepTime)
	if err != nil {
		return pm, err
	}
	if len(devOps) == 0 {
		// No communication ops — Python keeps only kernel host + kernel duration.
		pm.ZPDevice = invalidData
		pm.ZPDuration = invalidData
		pm.ZPKernel, _ = GetAvgKernelTaskDuration(db, stepTime)
		pm.DataLoader, _ = queryDataLoaderDuration(db, dlID, stepTime)
		if hostDurs := collectKernelHostDurations(db, kernelConnIDs); len(hostDurs) > 0 {
			pm.ZPHost, _ = CalculateMean(hostDurs)
		} else {
			pm.ZPHost = 0
		}
		return pm, nil
	}

	// 5. Collect unique connection IDs.
	connSet := make(map[int]bool)
	for _, op := range devOps {
		if op.ConnectionID > 0 {
			connSet[op.ConnectionID] = true
		}
	}
	connIDs := make([]int, 0, len(connSet))
	for cid := range connSet {
		connIDs = append(connIDs, cid)
	}

	// 6. Query host times. Keep the two tables separate: CANN_API wins over
	// MSTX_EVENTS for the same connection (Python precedence).
	cannMap, _ := getHostOpFromTable(db, "CANN_API", connIDs)
	mstxMap, _ := getHostOpFromTable(db, "MSTX_EVENTS", connIDs)

	// Pre-seed every domain to 0 (Python initializes durations/counts this way).
	for xp := range xpToGroupName {
		pm.Durations[xp] = 0
		pm.Counts[xp] = 0
	}

	// Fill host times into device ops and collect metrics.
	var commHostDurations []int
	var intervals []Interval
	domainOps := make(map[string][]OpStat)

	for i := range devOps {
		if ho, ok := cannMap[devOps[i].ConnectionID]; ok {
			devOps[i].HStartNs, devOps[i].HEndNs = ho.StartNs, ho.EndNs
		} else if ho, ok := mstxMap[devOps[i].ConnectionID]; ok {
			devOps[i].HStartNs, devOps[i].HEndNs = ho.StartNs, ho.EndNs
		}
		// Host duration.
		if devOps[i].HStartNs > 0 && devOps[i].HEndNs >= devOps[i].HStartNs {
			commHostDurations = append(commHostDurations, devOps[i].HEndNs-devOps[i].HStartNs)
		}
		// Communication intervals.
		if devOps[i].StartNs > 0 && devOps[i].EndNs >= devOps[i].StartNs {
			intervals = append(intervals, Interval{Start: devOps[i].StartNs, End: devOps[i].EndNs})
		}
		if xpName, ok := idToXp[devOps[i].DomainID]; ok {
			dur := devOps[i].EndNs - devOps[i].StartNs
			if dur >= 0 && devOps[i].Count >= 0 {
				domainOps[xpName] = append(domainOps[xpName], OpStat{Duration: dur, Count: devOps[i].Count})
			}
		}
	}

	// ZP_Host = mean(comm host + kernel host); HostDuration = mean(comm host only).
	kernelHostDurs := collectKernelHostDurations(db, kernelConnIDs)
	allHostDurations := make([]int, 0, len(commHostDurations)+len(kernelHostDurs))
	allHostDurations = append(allHostDurations, commHostDurations...)
	allHostDurations = append(allHostDurations, kernelHostDurs...)

	// 7. Compute metrics.
	pm.ZPDuration = mergeIntervalsSimple(intervals)
	pm.ZPDevice = stepTime.EndNs - stepTime.StartNs - pm.ZPDuration
	if pm.ZPDevice < 0 {
		pm.ZPDevice = 0
		fmt.Fprintf(os.Stderr, "[WARN] Communication time exceeds step time, ZP_Device clamped to 0\n")
	}

	pm.ZPHost = 0
	if len(allHostDurations) > 0 {
		pm.ZPHost, _ = CalculateMean(allHostDurations)
	}
	pm.HostDuration = 0
	if len(commHostDurations) > 0 {
		pm.HostDuration, _ = CalculateMean(commHostDurations)
	}
	pm.ZPKernel, _ = GetAvgKernelTaskDuration(db, stepTime)
	pm.DataLoader, _ = queryDataLoaderDuration(db, dlID, stepTime)
	pm.ZPCount = 0

	// Per-domain metrics: whitelist + plain mean.
	for domain, stats := range domainOps {
		if !domainAllowed(domain) {
			continue
		}
		if dur, cnt, err := CalculateMeanPair(stats); err == nil {
			pm.Durations[domain] = dur
			pm.Counts[domain] = cnt
		}
	}

	pm.MemcpyAsync, _ = getAvgTaskDurationByName(db, stepTime, "MEMCPY_ASYNC")
	pm.MemcpyCount, pm.MemcpyLinked, pm.MemcpyExternal = getMemcpyCounters(db, stepTime)
	pm.KernelAivec, _ = getAvgTaskDurationByName(db, stepTime, "KERNEL_AIVEC")

	return pm, nil
}

// GetAvgKernelTaskDuration returns the average duration of KERNEL_AICORE tasks
// within the given step window.
func GetAvgKernelTaskDuration(db *sql.DB, stepTime StepTime) (int, error) {
	var avg sql.NullFloat64
	err := db.QueryRow(`
		SELECT AVG(t.endNs - t.startNs)
		FROM TASK t
		INNER JOIN STRING_IDS s ON t.taskType = s.id
		WHERE s.value IN ('KERNEL_AICORE')
		  AND t.startNs >= ?
		  AND t.endNs <= ?
	`, stepTime.StartNs, stepTime.EndNs).Scan(&avg)
	if err != nil || !avg.Valid {
		return 0, fmt.Errorf("kernel avg query failed: %w", err)
	}
	return int(math.Round(avg.Float64)), nil
}

// ---------------------------------------------------------------------------
// SQL helpers
// ---------------------------------------------------------------------------

func queryDataLoaderID(db *sql.DB) (int, error) {
	var id int
	err := db.QueryRow("SELECT id FROM STRING_IDS WHERE value = 'dataloader'").Scan(&id)
	return id, err
}

func queryDataLoaderDuration(db *sql.DB, dlID int, stepTime StepTime) (int, error) {
	var start, end int
	err := db.QueryRow(`
		SELECT startNs, endNs FROM MSTX_EVENTS
		WHERE message = ? AND startNs >= ? AND endNs <= ?
		LIMIT 1
	`, strconv.Itoa(dlID), stepTime.StartNs, stepTime.EndNs).Scan(&start, &end)
	if err != nil {
		return 0, err
	}
	if end > start {
		return end - start, nil
	}
	return 0, nil
}

func batchQueryStringIDs(db *sql.DB, values []string) (map[string]int, error) {
	if len(values) == 0 {
		return nil, fmt.Errorf("no values")
	}
	args := make([]interface{}, len(values))
	for i, v := range values {
		args[i] = v
	}
	query := fmt.Sprintf("SELECT value, id FROM STRING_IDS WHERE value IN (%s)", placeholders(len(values)))
	rows, err := db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	result := make(map[string]int)
	for rows.Next() {
		var val string
		var id int
		if err := rows.Scan(&val, &id); err != nil {
			continue
		}
		result[val] = id
	}
	return result, rows.Err()
}

func getDeviceOpList(db *sql.DB, groupNameIDs []int, stepTime StepTime) ([]CommunicationOp, error) {
	if len(groupNameIDs) == 0 {
		return nil, nil
	}
	// Build IN clause parameters.
	placeholders := make([]string, len(groupNameIDs))
	args := make([]interface{}, 0, len(groupNameIDs)+2)
	for i, gid := range groupNameIDs {
		placeholders[i] = "?"
		args = append(args, gid)
	}
	args = append(args, stepTime.StartNs, stepTime.EndNs)

	query := fmt.Sprintf(`
		SELECT opName, startNs, endNs, connectionId, count, _rowid_, groupName
		FROM COMMUNICATION_OP
		WHERE groupName IN (%s)
		  AND startNs >= ?
		  AND endNs <= ?
		ORDER BY startNs ASC
	`, strings.Join(placeholders, ", "))

	rows, err := db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var ops []CommunicationOp
	for rows.Next() {
		var op CommunicationOp
		if err := rows.Scan(&op.OpName, &op.StartNs, &op.EndNs,
			&op.ConnectionID, &op.Count, &op.OpStreamIndex, &op.DomainID); err != nil {
			continue
		}
		ops = append(ops, op)
	}
	return ops, rows.Err()
}

func getHostOpFromTable(db *sql.DB, tableName string, connIDs []int) (map[int]HostOp, error) {
	if len(connIDs) == 0 {
		return nil, nil
	}
	// Check table exists.
	exists, _ := tableExists(db, tableName)
	if !exists {
		return nil, nil
	}

	placeholders := make([]string, len(connIDs))
	args := make([]interface{}, len(connIDs))
	for i, cid := range connIDs {
		placeholders[i] = "?"
		args[i] = cid
	}

	query := fmt.Sprintf(
		"SELECT startNs, endNs, connectionId FROM %s WHERE connectionId IN (%s)",
		tableName, strings.Join(placeholders, ", "),
	)
	rows, err := db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	result := make(map[int]HostOp)
	for rows.Next() {
		var ho HostOp
		var cid int
		if err := rows.Scan(&ho.StartNs, &ho.EndNs, &cid); err != nil {
			continue
		}
		result[cid] = ho
	}
	return result, rows.Err()
}

func getKernelAicoreTaskConnectionIDs(db *sql.DB, stepTime StepTime) ([]int, error) {
	rows, err := db.Query(`
		SELECT t.connectionId
		FROM TASK t
		INNER JOIN STRING_IDS s ON t.taskType = s.id
		WHERE s.value IN ('KERNEL_AICORE')
		  AND t.startNs >= ?
		  AND t.endNs <= ?
		  AND t.connectionId > 0
	`, stepTime.StartNs, stepTime.EndNs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var ids []int
	for rows.Next() {
		var cid int
		if err := rows.Scan(&cid); err != nil {
			continue
		}
		ids = append(ids, cid)
	}
	return ids, rows.Err()
}

func collectKernelHostDurations(db *sql.DB, kernelConnIDs []int) []int {
	if len(kernelConnIDs) == 0 {
		return nil
	}
	cann, _ := getHostOpFromTable(db, "CANN_API", kernelConnIDs)
	mstx, _ := getHostOpFromTable(db, "MSTX_EVENTS", kernelConnIDs)
	return hostDurationsFor(kernelConnIDs, cann, mstx)
}

// getAvgTaskDurationByName returns AVG(endNs-startNs) for tasks whose
// STRING_IDS value equals name; 0 when there are no rows.
func getAvgTaskDurationByName(db *sql.DB, stepTime StepTime, name string) (int, error) {
	var avg sql.NullFloat64
	err := db.QueryRow(`
		SELECT AVG(t.endNs - t.startNs)
		FROM TASK t
		INNER JOIN STRING_IDS s ON t.taskType = s.id
		WHERE s.value = ?
		  AND t.startNs >= ?
		  AND t.endNs <= ?
	`, name, stepTime.StartNs, stepTime.EndNs).Scan(&avg)
	if err != nil || !avg.Valid {
		return 0, nil
	}
	return int(math.Round(avg.Float64)), nil
}

// getMemcpyCounters returns (total MEMCPY_ASYNC tasks, tasks linked to CANN_API,
// tasks whose API is aclrtMemcpyAsync). When CANN_API is missing the latter two
// are -1 ("unavailable"), matching Python.
func getMemcpyCounters(db *sql.DB, stepTime StepTime) (int, int, int) {
	rows, err := db.Query(`
		SELECT t.connectionId
		FROM TASK t
		INNER JOIN STRING_IDS s ON t.taskType = s.id
		WHERE s.value = 'MEMCPY_ASYNC'
		  AND t.startNs >= ?
		  AND t.endNs <= ?
	`, stepTime.StartNs, stepTime.EndNs)
	if err != nil {
		return 0, -1, -1
	}
	defer rows.Close()

	var cids []int
	for rows.Next() {
		var cid sql.NullInt64
		if err := rows.Scan(&cid); err != nil {
			continue
		}
		if cid.Valid {
			cids = append(cids, int(cid.Int64))
		}
	}
	total := len(cids)
	if total == 0 {
		return 0, -1, -1
	}
	exists, _ := tableExists(db, "CANN_API")
	if !exists {
		return total, -1, -1
	}

	placeholders := make([]string, len(cids))
	args := make([]interface{}, len(cids))
	for i, cid := range cids {
		placeholders[i] = "?"
		args[i] = cid
	}
	query := fmt.Sprintf("SELECT connectionId, name FROM CANN_API WHERE connectionId IN (%s)", strings.Join(placeholders, ", "))
	nameRows, err := db.Query(query, args...)
	if err != nil {
		return total, -1, -1
	}
	defer nameRows.Close()

	cidToNameID := make(map[int]int)
	var nameIDs []int
	for nameRows.Next() {
		var cid int
		var nameID sql.NullInt64
		if err := nameRows.Scan(&cid, &nameID); err != nil {
			continue
		}
		if nameID.Valid {
			cidToNameID[cid] = int(nameID.Int64)
			nameIDs = append(nameIDs, int(nameID.Int64))
		}
	}
	idToName := stringMapByIDs(db, nameIDs)

	linked, external := 0, 0
	for _, cid := range cids {
		if nid, ok := cidToNameID[cid]; ok {
			linked++
			if idToName[nid] == "aclrtMemcpyAsync" {
				external++
			}
		}
	}
	return total, linked, external
}

func tableExists(db *sql.DB, tableName string) (bool, error) {
	var count int
	err := db.QueryRow(
		"SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?",
		tableName,
	).Scan(&count)
	return count > 0, err
}

// CheckTableExists is the public wrapper.
func CheckTableExists(db *sql.DB, tableName string) (bool, error) {
	return tableExists(db, tableName)
}

// ---------------------------------------------------------------------------
// CSV I/O
// ---------------------------------------------------------------------------

// WriteResultsToCSV writes a slice of PerformanceMetrics to a CSV file.
// All rows in a single call share the same set of headers.
func WriteResultsToCSV(outputFile string, metrics []PerformanceMetrics) error {
	if len(metrics) == 0 {
		return nil
	}

	// Collect all domain names (sorted for determinism).
	domainSet := make(map[string]bool)
	for _, m := range metrics {
		for d := range m.Durations {
			domainSet[d] = true
		}
		for d := range m.Counts {
			domainSet[d] = true
		}
	}
	domains := make([]string, 0, len(domainSet))
	for d := range domainSet {
		domains = append(domains, d)
	}
	sort.Strings(domains)

	// Build header (Python level0 column order).
	header := []string{
		"StepIndex", "StepDuration", "ZP_Device", "ZP_Duration",
		"ZP_Host", "ZP_Count", "KERNEL_AICORE",
		"MEMCPY_ASYNC", "KERNEL_AIVEC", "HostDuration", "DataLoader",
	}
	for _, d := range domains {
		header = append(header, d+"_Duration", d+"_Count")
	}
	header = append(header, "MEMCPY_ASYNC_Count", "MEMCPY_LINKED_Count", "MEMCPY_EXTERNAL_Count")

	// Ensure output directory exists.
	os.MkdirAll(filepath.Dir(outputFile), 0755)

	csvMutex.Lock()
	defer csvMutex.Unlock()

	f, err := os.Create(outputFile)
	if err != nil {
		return err
	}
	defer f.Close()

	w := csv.NewWriter(f)
	if err := w.Write(header); err != nil {
		return err
	}

	for _, m := range metrics {
		row := []string{
			strconv.Itoa(m.StepIndex),
			strconv.Itoa(m.StepDuration),
			strconv.Itoa(m.ZPDevice),
			strconv.Itoa(m.ZPDuration),
			strconv.Itoa(m.ZPHost),
			strconv.Itoa(m.ZPCount),
			strconv.Itoa(m.ZPKernel),
			strconv.Itoa(m.MemcpyAsync),
			strconv.Itoa(m.KernelAivec),
			strconv.Itoa(m.HostDuration),
			strconv.Itoa(m.DataLoader),
		}
		for _, d := range domains {
			row = append(row, strconv.Itoa(m.Durations[d]), strconv.Itoa(m.Counts[d]))
		}
		row = append(row, strconv.Itoa(m.MemcpyCount), strconv.Itoa(m.MemcpyLinked), strconv.Itoa(m.MemcpyExternal))
		if err := w.Write(row); err != nil {
			return err
		}
	}
	w.Flush()
	return w.Error()
}
