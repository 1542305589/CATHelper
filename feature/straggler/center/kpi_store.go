package center

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

// kpiSample is one daemon-reported KPI sample: timestamp + device → metric → value.
// CPU samples are intentionally not shipped by the daemon, so Vals is the only data.
type kpiSample struct {
	Ts   int64                         `json:"ts"`
	Vals map[string]map[string]float64 `json:"vals,omitempty"`
}

// kpiReport is the daemon → center POST body for /center/kpi/{business}/{daemon}.
type kpiReport struct {
	Node    string      `json:"node"`
	SentAt  int64       `json:"sent_at,omitempty"`
	Samples []kpiSample `json:"samples"`
}

// kpiRow is one stored device sample, returned by latest/history queries.
type kpiRow struct {
	Daemon  string             `json:"daemon"`
	Node    string             `json:"node"`
	Ts      int64              `json:"ts"`
	Device  int                `json:"device"`
	Metrics map[string]float64 `json:"metrics"`
}

// kpiFilter is one node's filter. Devices is the allowlist; FilterAll means every
// device on that node is filtered out. An absent entry means "no filter" (all
// devices allowed).
type kpiFilter struct {
	Node      string `json:"node"`
	Devices   []int  `json:"devices"`
	FilterAll bool   `json:"filter_all,omitempty"`
}

// kpiStore persists KPI time series in SQLite. The full history is kept; latest
// queries are served by a per-(daemon,node,device) MAX(ts) join. Filtering is a
// query-time allowlist stored in kpi_card_filter so changing a filter never
// destroys raw data.
type kpiStore struct {
	db      *sql.DB
	logf    func(format string, args ...any)
	writeMu sync.Mutex // serialize writers; SQLite allows only one writer

	seriesMu    sync.Mutex
	seriesCache map[string]kpiSeriesCacheEntry
}

type kpiSeriesCacheEntry struct {
	at   time.Time
	rows []kpiSeriesRow
}

func newKPIStore(path string, logf func(format string, args ...any)) (*kpiStore, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("mkdir kpi db dir: %w", err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open kpi db: %w", err)
	}
	db.SetMaxOpenConns(8)
	for _, pragma := range []string{
		"PRAGMA journal_mode=WAL",
		"PRAGMA busy_timeout=30000",
		"PRAGMA synchronous=NORMAL",
		"PRAGMA temp_store=MEMORY",
		"PRAGMA cache_size=-65536",
		"PRAGMA mmap_size=268435456",
	} {
		if _, err := db.Exec(pragma); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("%s: %w", pragma, err)
		}
	}
	schema := []string{
		`CREATE TABLE IF NOT EXISTS kpi_samples (
			business TEXT NOT NULL,
			daemon TEXT NOT NULL,
			node TEXT NOT NULL,
			ts INTEGER NOT NULL,
			device INTEGER NOT NULL,
			metrics TEXT NOT NULL,
			received_at INTEGER NOT NULL,
			PRIMARY KEY (business, daemon, ts, device)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_kpi_samples_business_ts ON kpi_samples(business, ts)`,
		`CREATE INDEX IF NOT EXISTS idx_kpi_samples_node_ts ON kpi_samples(business, node, ts)`,
		`CREATE INDEX IF NOT EXISTS idx_kpi_samples_device_ts ON kpi_samples(business, node, device, ts)`,
		`CREATE TABLE IF NOT EXISTS kpi_meta (
			key TEXT PRIMARY KEY,
			value TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS kpi_card_filter (
			business TEXT NOT NULL,
			node TEXT NOT NULL,
			device INTEGER NOT NULL,
			PRIMARY KEY (business, node, device)
		)`,
		`CREATE TABLE IF NOT EXISTS kpi_filter_all (
			business TEXT NOT NULL,
			node TEXT NOT NULL,
			PRIMARY KEY (business, node)
		)`,
		`CREATE TABLE IF NOT EXISTS kpi_anomaly (
			business TEXT NOT NULL,
			node TEXT NOT NULL,
			device INTEGER NOT NULL,
			ts INTEGER NOT NULL,
			metric TEXT NOT NULL,
			kind TEXT NOT NULL,
			value REAL NOT NULL,
			median REAL NOT NULL,
			mad REAL NOT NULL,
			z REAL NOT NULL,
			reference_admitted INTEGER NOT NULL DEFAULT 0,
			created_at INTEGER NOT NULL,
			PRIMARY KEY (business, node, device, ts, metric, kind)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_kpi_anomaly_lookup ON kpi_anomaly(business, node, device, ts)`,
		`CREATE TABLE IF NOT EXISTS kpi_detect_run (
			business TEXT NOT NULL,
			node TEXT NOT NULL,
			ts INTEGER NOT NULL,
			devices INTEGER NOT NULL,
			anomalies INTEGER NOT NULL,
			flagged INTEGER NOT NULL,
			reference_invalid INTEGER NOT NULL,
			global_shift INTEGER NOT NULL DEFAULT 0,
			created_at INTEGER NOT NULL,
			PRIMARY KEY (business, node, ts)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_kpi_detect_run_business_ts ON kpi_detect_run(business, ts)`,
	}
	for _, stmt := range schema {
		if _, err := db.Exec(stmt); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("kpi schema: %w", err)
		}
	}
	if err := ensureKPIAnomalyReferenceColumn(db); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err := ensureKPIrunGlobalShiftColumn(db); err != nil {
		_ = db.Close()
		return nil, err
	}
	// Older rows were stored with the raw hostname; annotate them in place with
	// the reporting daemon so two daemons sharing a hostname stay distinct. Run
	// once via kpi_meta to avoid scanning a large history on every startup.
	var migrated int
	_ = db.QueryRow(`SELECT COUNT(*) FROM kpi_meta WHERE key = 'node_annotated_v1'`).Scan(&migrated)
	if migrated == 0 {
		if _, err := db.Exec(`UPDATE kpi_samples
			SET node = node || '(' || daemon || ')'
			WHERE instr(node, '(' || daemon || ')') = 0`); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("kpi node migration: %w", err)
		}
		if _, err := db.Exec(`INSERT OR REPLACE INTO kpi_meta (key, value) VALUES ('node_annotated_v1', '1')`); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("kpi migration marker: %w", err)
		}
	}
	return &kpiStore{db: db, logf: logf, seriesCache: make(map[string]kpiSeriesCacheEntry)}, nil
}

func (s *kpiStore) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

// Insert stores every (ts, device) sample for one daemon. Duplicate reports are
// idempotent thanks to INSERT OR REPLACE on the primary key.
func (s *kpiStore) Insert(business, daemon, node string, samples []kpiSample) (int, error) {
	if s == nil || s.db == nil {
		return 0, fmt.Errorf("kpi store disabled")
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()

	stmt, err := tx.Prepare(`INSERT OR REPLACE INTO kpi_samples
		(business, daemon, node, ts, device, metrics, received_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return 0, err
	}
	defer stmt.Close()

	now := time.Now().Unix()
	rows := 0
	for _, sample := range samples {
		if sample.Ts <= 0 || len(sample.Vals) == 0 {
			continue
		}
		for deviceStr, metrics := range sample.Vals {
			device, err := strconv.Atoi(deviceStr)
			if err != nil || device < 0 {
				continue
			}
			raw, err := json.Marshal(metrics)
			if err != nil {
				continue
			}
			if _, err := stmt.Exec(business, daemon, node, sample.Ts, device, string(raw), now); err != nil {
				return rows, err
			}
			rows++
		}
	}
	if err := tx.Commit(); err != nil {
		return rows, err
	}
	s.clearSeriesCache()
	return rows, nil
}

// Latest returns the newest sample per (daemon, node, device) for a business,
// after applying the stored card allowlist.
func (s *kpiStore) Latest(business, node string, device *int) ([]kpiRow, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("kpi store disabled")
	}
	query := `SELECT k.daemon, k.node, k.ts, k.device, k.metrics
		FROM kpi_samples k
		JOIN (
			SELECT daemon, node, device, MAX(ts) AS max_ts
			FROM kpi_samples WHERE business = ?
			GROUP BY daemon, node, device
		) m ON k.daemon = m.daemon AND k.node = m.node
			AND k.device = m.device AND k.ts = m.max_ts
		WHERE k.business = ?`
	args := []any{business, business}
	filterSQL, filterArgs := kpiFilterSQL("k", node, device)
	query += " AND " + filterSQL
	args = append(args, filterArgs...)
	query += " ORDER BY k.node, k.device"

	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanKPIRows(rows)
}

// History returns samples in [from, to] after applying the card allowlist. The
// result is oldest-first; limit caps the number of returned rows and keeps the
// newest samples inside the range.
func (s *kpiStore) History(business string, from, to int64, node string, device *int, limit int) ([]kpiRow, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("kpi store disabled")
	}
	if limit <= 0 {
		limit = 1000
	}
	if limit > 100000 {
		limit = 100000
	}
	query := `SELECT k.daemon, k.node, k.ts, k.device, k.metrics
		FROM kpi_samples k
		WHERE k.business = ? AND k.ts >= ? AND k.ts <= ?`
	args := []any{business, from, to}
	filterSQL, filterArgs := kpiFilterSQL("k", node, device)
	query += " AND " + filterSQL
	args = append(args, filterArgs...)
	query += " ORDER BY k.ts DESC LIMIT ?"
	args = append(args, limit)

	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out, err := scanKPIRows(rows)
	if err != nil {
		return nil, err
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, nil
}

// SetFilter replaces the filter for one (business, node). filterAll means every
// device is filtered out; otherwise devices is the allowlist and an empty slice
// removes the filter (all devices allowed).
func (s *kpiStore) SetFilter(business, node string, devices []int, filterAll bool) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("kpi store disabled")
	}
	if node == "" {
		return fmt.Errorf("node is required")
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(`DELETE FROM kpi_card_filter WHERE business = ? AND node = ?`, business, node); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM kpi_filter_all WHERE business = ? AND node = ?`, business, node); err != nil {
		return err
	}
	if filterAll {
		if _, err := tx.Exec(`INSERT OR IGNORE INTO kpi_filter_all (business, node) VALUES (?, ?)`, business, node); err != nil {
			return err
		}
		return tx.Commit()
	}
	stmt, err := tx.Prepare(`INSERT OR IGNORE INTO kpi_card_filter (business, node, device) VALUES (?, ?, ?)`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	seen := make(map[int]bool, len(devices))
	for _, device := range devices {
		if device < 0 || seen[device] {
			continue
		}
		seen[device] = true
		if _, err := stmt.Exec(business, node, device); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// Devices returns the node/device pairs present in the latest KPI data for a
// business, ignoring any allowlist. It is used by the console to render the
// selectable card list.
func (s *kpiStore) Devices(business string) ([]kpiFilter, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("kpi store disabled")
	}
	query := `SELECT DISTINCT k.node, k.device
		FROM kpi_samples k
		JOIN (
			SELECT daemon, node, device, MAX(ts) AS max_ts
			FROM kpi_samples WHERE business = ?
			GROUP BY daemon, node, device
		) m ON k.daemon = m.daemon AND k.node = m.node
			AND k.device = m.device AND k.ts = m.max_ts
		WHERE k.business = ?
		ORDER BY k.node, k.device`
	rows, err := s.db.Query(query, business, business)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	byNode := make(map[string][]int)
	var order []string
	for rows.Next() {
		var node string
		var device int
		if err := rows.Scan(&node, &device); err != nil {
			return nil, err
		}
		if _, ok := byNode[node]; !ok {
			order = append(order, node)
		}
		byNode[node] = append(byNode[node], device)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]kpiFilter, 0, len(order))
	for _, node := range order {
		out = append(out, kpiFilter{Node: node, Devices: byNode[node]})
	}
	return out, nil
}

// Filters returns the stored card allowlists for a business.
func (s *kpiStore) Filters(business string) ([]kpiFilter, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("kpi store disabled")
	}
	rows, err := s.db.Query(`SELECT node, device FROM kpi_card_filter WHERE business = ? ORDER BY node, device`, business)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	byNode := make(map[string][]int)
	var order []string
	for rows.Next() {
		var node string
		var device int
		if err := rows.Scan(&node, &device); err != nil {
			return nil, err
		}
		if _, ok := byNode[node]; !ok {
			order = append(order, node)
		}
		byNode[node] = append(byNode[node], device)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	allRows, err := s.db.Query(`SELECT node FROM kpi_filter_all WHERE business = ? ORDER BY node`, business)
	if err != nil {
		return nil, err
	}
	defer allRows.Close()
	allNodes := make(map[string]bool)
	for allRows.Next() {
		var node string
		if err := allRows.Scan(&node); err != nil {
			return nil, err
		}
		if _, ok := byNode[node]; !ok {
			order = append(order, node)
		}
		allNodes[node] = true
	}
	if err := allRows.Err(); err != nil {
		return nil, err
	}

	out := make([]kpiFilter, 0, len(order))
	for _, node := range order {
		devices := byNode[node]
		if devices == nil {
			devices = []int{}
		}
		out = append(out, kpiFilter{Node: node, Devices: devices, FilterAll: allNodes[node]})
	}
	return out, nil
}

// DeleteBusiness removes all KPI samples and filters for a business.
func (s *kpiStore) DeleteBusiness(business string) error {
	if s == nil || s.db == nil {
		return nil
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(`DELETE FROM kpi_samples WHERE business = ?`, business); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM kpi_card_filter WHERE business = ?`, business); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM kpi_filter_all WHERE business = ?`, business); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM kpi_anomaly WHERE business = ?`, business); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM kpi_detect_run WHERE business = ?`, business); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.clearSeriesCache()
	return nil
}

func scanKPIRows(rows *sql.Rows) ([]kpiRow, error) {
	out := make([]kpiRow, 0)
	for rows.Next() {
		var row kpiRow
		var raw string
		if err := rows.Scan(&row.Daemon, &row.Node, &row.Ts, &row.Device, &raw); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(raw), &row.Metrics); err != nil {
			row.Metrics = map[string]float64{}
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// kpiFilterSQL returns the allowlist predicate for alias (a table alias). If a
// node has any filter rows, only those devices are allowed; otherwise all devices
// on that node are allowed.
func kpiFilterSQL(alias, node string, device *int) (string, []any) {
	clause := `NOT EXISTS (
			SELECT 1 FROM kpi_filter_all a
			WHERE a.business = ` + alias + `.business AND a.node = ` + alias + `.node
		) AND (NOT EXISTS (
			SELECT 1 FROM kpi_card_filter f
			WHERE f.business = ` + alias + `.business AND f.node = ` + alias + `.node
		) OR EXISTS (
			SELECT 1 FROM kpi_card_filter f
			WHERE f.business = ` + alias + `.business AND f.node = ` + alias + `.node
				AND f.device = ` + alias + `.device
		))`
	args := []any{}
	if node != "" {
		clause += " AND " + alias + ".node = ?"
		args = append(args, node)
	}
	if device != nil {
		clause += " AND " + alias + ".device = ?"
		args = append(args, *device)
	}
	return clause, args
}

// parseDeviceFilter parses an optional ?device=N query parameter.
func parseDeviceFilter(raw string) (*int, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 0 {
		return nil, fmt.Errorf("invalid device %q", raw)
	}
	return &n, nil
}

// kpiStoredSample is one raw KPI row returned by RecentSamples.
type kpiStoredSample struct {
	Device  int
	Ts      int64
	Metrics map[string]float64
}

// kpiRefPoint is one historical point used to build the reference window.
type kpiRefPoint struct {
	Ts      int64
	Metrics map[string]float64
}

// kpiAnomaly is one detected time/space anomaly.
func ensureKPIAnomalyReferenceColumn(db *sql.DB) error {
	rows, err := db.Query(`PRAGMA table_info(kpi_anomaly)`)
	if err != nil {
		return err
	}
	defer rows.Close()
	has := false
	for rows.Next() {
		var cid int
		var name, ctype string
		var notnull, pk int
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			return err
		}
		if name == "reference_admitted" {
			has = true
		}
	}
	if has {
		return nil
	}
	_, err = db.Exec(`ALTER TABLE kpi_anomaly ADD COLUMN reference_admitted INTEGER NOT NULL DEFAULT 0`)
	return err
}

func ensureKPIrunGlobalShiftColumn(db *sql.DB) error {
	rows, err := db.Query(`PRAGMA table_info(kpi_detect_run)`)
	if err != nil {
		return err
	}
	defer rows.Close()
	has := false
	for rows.Next() {
		var cid int
		var name, ctype string
		var notnull, pk int
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			return err
		}
		if name == "global_shift" {
			has = true
		}
	}
	if has {
		return nil
	}
	_, err = db.Exec(`ALTER TABLE kpi_detect_run ADD COLUMN global_shift INTEGER NOT NULL DEFAULT 0`)
	return err
}

type kpiAnomaly struct {
	Business          string  `json:"business"`
	Node              string  `json:"node"`
	Device            int     `json:"device"`
	Ts                int64   `json:"ts"`
	Metric            string  `json:"metric"`
	Kind              string  `json:"kind"` // "time" or "space"
	Value             float64 `json:"value"`
	Median            float64 `json:"median"`
	Mad               float64 `json:"mad"`
	Z                 float64 `json:"z"`
	ReferenceAdmitted bool    `json:"-"`
}

// kpiSeriesRow is one downsampled (1-minute) chart point.
type kpiSeriesRow struct {
	Node   string
	Device int
	Ts     int64
	Power  *float64
	Util   *float64
	Freq   *float64
}

// Series returns downsampled KPI points for a business in [from, to]. step is the
// bucket size in seconds (5 = raw 5s samples, 60 = 1-minute buckets).
func (s *kpiStore) Series(business string, from, to int64, step int64) ([]kpiSeriesRow, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("kpi store disabled")
	}
	if step <= 0 {
		step = 5
	}
	cacheKey := business + "|" + strconv.FormatInt(step, 10) + "|" + strconv.FormatInt(from/step, 10)
	s.seriesMu.Lock()
	if e, ok := s.seriesCache[cacheKey]; ok && time.Since(e.at) < 30*time.Second {
		rows := e.rows
		s.seriesMu.Unlock()
		return rows, nil
	}
	s.seriesMu.Unlock()
	rows, err := s.db.Query(`SELECT node, device, (ts / ?) * ? AS bucket,
		AVG(json_extract(metrics, '$.power')),
		AVG(json_extract(metrics, '$.aicore_util')),
		AVG(json_extract(metrics, '$.aicore_freq'))
		FROM kpi_samples
		WHERE business = ? AND ts >= ? AND ts <= ?
		GROUP BY node, device, bucket
		ORDER BY node, device, bucket`, step, step, business, from, to)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]kpiSeriesRow, 0)
	for rows.Next() {
		var row kpiSeriesRow
		var power, util, freq sql.NullFloat64
		if err := rows.Scan(&row.Node, &row.Device, &row.Ts, &power, &util, &freq); err != nil {
			return nil, err
		}
		if power.Valid {
			v := power.Float64
			row.Power = &v
		}
		if util.Valid {
			v := util.Float64
			row.Util = &v
		}
		if freq.Valid {
			v := freq.Float64
			row.Freq = &v
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	s.seriesMu.Lock()
	if len(s.seriesCache) >= 64 {
		s.seriesCache = make(map[string]kpiSeriesCacheEntry)
	}
	s.seriesCache[cacheKey] = kpiSeriesCacheEntry{at: time.Now(), rows: out}
	s.seriesMu.Unlock()
	return out, nil
}

// clearSeriesCache drops cached chart series after a write.
func (s *kpiStore) clearSeriesCache() {
	if s == nil {
		return
	}
	s.seriesMu.Lock()
	s.seriesCache = make(map[string]kpiSeriesCacheEntry)
	s.seriesMu.Unlock()
}

// NodesDevices returns the node → devices map observed for a business.
func (s *kpiStore) NodesDevices(business string) (map[string][]int, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("kpi store disabled")
	}
	rows, err := s.db.Query(`SELECT DISTINCT node, device FROM kpi_samples WHERE business = ? ORDER BY node, device`, business)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string][]int)
	for rows.Next() {
		var node string
		var device int
		if err := rows.Scan(&node, &device); err != nil {
			return nil, err
		}
		out[node] = append(out[node], device)
	}
	return out, rows.Err()
}

// LatestSampleTs returns the newest raw KPI timestamp for one node.
func (s *kpiStore) LatestSampleTs(business, node string) (int64, error) {
	if s == nil || s.db == nil {
		return 0, fmt.Errorf("kpi store disabled")
	}
	var ts sql.NullInt64
	if err := s.db.QueryRow(`SELECT MAX(ts) FROM kpi_samples WHERE business = ? AND node = ?`, business, node).Scan(&ts); err != nil {
		return 0, err
	}
	if !ts.Valid {
		return 0, nil
	}
	return ts.Int64, nil
}

// RecentSamples returns raw KPI rows at or after since for one node.
func (s *kpiStore) RecentSamples(business, node string, since int64) ([]kpiStoredSample, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("kpi store disabled")
	}
	rows, err := s.db.Query(`SELECT device, ts, metrics FROM kpi_samples
		WHERE business = ? AND node = ? AND ts >= ? ORDER BY ts`, business, node, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]kpiStoredSample, 0)
	for rows.Next() {
		var sample kpiStoredSample
		var raw string
		if err := rows.Scan(&sample.Device, &sample.Ts, &raw); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(raw), &sample.Metrics); err != nil {
			continue
		}
		out = append(out, sample)
	}
	return out, rows.Err()
}

// ReferenceSamples returns the last limit historical rows for one device before
// beforeTs, excluding any (device, ts) window already marked anomalous.
func (s *kpiStore) ReferenceSamples(business, node string, device int, sinceTs, beforeTs int64, excludeWindowSec int64, limit int) ([]kpiRefPoint, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("kpi store disabled")
	}
	if limit <= 0 {
		limit = 10000
	}
	if excludeWindowSec < 1 {
		excludeWindowSec = 1
	}
	query := `SELECT s.ts, s.metrics FROM kpi_samples s
		WHERE s.business = ? AND s.node = ? AND s.device = ? AND s.ts < ?
		AND NOT EXISTS (
			SELECT 1 FROM kpi_anomaly a
			WHERE a.business = s.business AND a.node = s.node AND a.device = s.device
				AND s.ts > a.ts - ? AND s.ts <= a.ts
				AND a.reference_admitted = 0
		)`
	args := []any{business, node, device, beforeTs, excludeWindowSec}
	if sinceTs > 0 {
		query += " AND s.ts >= ?"
		args = append(args, sinceTs)
	}
	query += " ORDER BY s.ts DESC LIMIT ?"
	args = append(args, limit)
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]kpiRefPoint, 0, limit)
	for rows.Next() {
		var point kpiRefPoint
		var raw string
		if err := rows.Scan(&point.Ts, &raw); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(raw), &point.Metrics); err != nil {
			continue
		}
		out = append(out, point)
	}
	return out, rows.Err()
}

// InsertAnomalies stores detected anomalies. Duplicate reports are idempotent.
func (s *kpiStore) InsertAnomalies(items []kpiAnomaly) error {
	if s == nil || s.db == nil || len(items) == 0 {
		return nil
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	var err error
	for attempt := 0; attempt < 5; attempt++ {
		err = s.insertAnomaliesOnce(items)
		if err == nil || !strings.Contains(err.Error(), "locked") {
			return err
		}
		time.Sleep(200 * time.Millisecond)
	}
	return err
}

func (s *kpiStore) insertAnomaliesOnce(items []kpiAnomaly) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	stmt, err := tx.Prepare(`INSERT OR REPLACE INTO kpi_anomaly
		(business, node, device, ts, metric, kind, value, median, mad, z, reference_admitted, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	now := time.Now().Unix()
	for _, item := range items {
		if _, err := stmt.Exec(item.Business, item.Node, item.Device, item.Ts, item.Metric,
			item.Kind, item.Value, item.Median, item.Mad, item.Z, boolInt(item.ReferenceAdmitted), now); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// kpiDetectRun is one node's detection-cycle summary.
type kpiDetectRun struct {
	Business         string `json:"business"`
	Node             string `json:"node"`
	Ts               int64  `json:"ts"`
	Devices          int    `json:"devices"`
	Anomalies        int    `json:"anomalies"`
	Flagged          int    `json:"flagged"`
	ReferenceInvalid bool   `json:"reference_invalid"`
	GlobalShift      bool   `json:"global_shift"`
}

// InsertDetectRun records one detection cycle for a node.
func (s *kpiStore) InsertDetectRun(run kpiDetectRun) error {
	if s == nil || s.db == nil {
		return nil
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	_, err := s.db.Exec(`INSERT OR REPLACE INTO kpi_detect_run
		(business, node, ts, devices, anomalies, flagged, reference_invalid, global_shift, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		run.Business, run.Node, run.Ts, run.Devices, run.Anomalies, run.Flagged,
		boolInt(run.ReferenceInvalid), boolInt(run.GlobalShift), time.Now().Unix())
	return err
}

// DetectRuns returns recent detection-cycle summaries for a business.
func (s *kpiStore) DetectRuns(business string, limit int) ([]kpiDetectRun, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("kpi store disabled")
	}
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.db.Query(`SELECT business, node, ts, devices, anomalies, flagged, reference_invalid, global_shift
		FROM kpi_detect_run WHERE business = ? ORDER BY ts DESC LIMIT ?`, business, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]kpiDetectRun, 0)
	for rows.Next() {
		var run kpiDetectRun
		var invalid, shift int
		if err := rows.Scan(&run.Business, &run.Node, &run.Ts, &run.Devices, &run.Anomalies, &run.Flagged, &invalid, &shift); err != nil {
			return nil, err
		}
		run.ReferenceInvalid = invalid != 0
		run.GlobalShift = shift != 0
		out = append(out, run)
	}
	return out, rows.Err()
}

// DetectRunsRange returns detection-cycle summaries in [from, to], newest first.
func (s *kpiStore) DetectRunsRange(business string, from, to int64, limit int) ([]kpiDetectRun, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("kpi store disabled")
	}
	if limit <= 0 {
		limit = 5000
	}
	if limit > 20000 {
		limit = 20000
	}
	rows, err := s.db.Query(`SELECT business, node, ts, devices, anomalies, flagged, reference_invalid, global_shift
		FROM kpi_detect_run WHERE business = ? AND ts >= ? AND ts <= ?
		ORDER BY ts DESC LIMIT ?`, business, from, to, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]kpiDetectRun, 0)
	for rows.Next() {
		var run kpiDetectRun
		var invalid, shift int
		if err := rows.Scan(&run.Business, &run.Node, &run.Ts, &run.Devices, &run.Anomalies, &run.Flagged, &invalid, &shift); err != nil {
			return nil, err
		}
		run.ReferenceInvalid = invalid != 0
		run.GlobalShift = shift != 0
		out = append(out, run)
	}
	return out, rows.Err()
}

// AnomaliesAt returns anomaly rows for one business at one detection ts.
func (s *kpiStore) AnomaliesAt(business string, ts int64) ([]kpiAnomaly, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("kpi store disabled")
	}
	rows, err := s.db.Query(`SELECT business, node, device, ts, metric, kind, value, median, mad, z
		FROM kpi_anomaly WHERE business = ? AND ts = ? ORDER BY node, device, metric, kind`, business, ts)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]kpiAnomaly, 0)
	for rows.Next() {
		var a kpiAnomaly
		if err := rows.Scan(&a.Business, &a.Node, &a.Device, &a.Ts, &a.Metric, &a.Kind,
			&a.Value, &a.Median, &a.Mad, &a.Z); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// DeleteAnomaliesForMetric removes anomaly rows for a metric at specific ts.
func (s *kpiStore) DeleteAnomaliesForMetric(business, node, metric string, tsList []int64) error {
	if s == nil || s.db == nil || len(tsList) == 0 {
		return nil
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	stmt, err := tx.Prepare(`DELETE FROM kpi_anomaly WHERE business = ? AND node = ? AND metric = ? AND ts = ?`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, ts := range tsList {
		if _, err := stmt.Exec(business, node, metric, ts); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// PruneKPI deletes samples/anomalies/runs older than retention and reclaims space.
func (s *kpiStore) PruneKPI(retention time.Duration) (int64, error) {
	if s == nil || s.db == nil {
		return 0, nil
	}
	if retention <= 0 {
		retention = 48 * time.Hour
	}
	cutoff := time.Now().Add(-retention).Unix()
	s.writeMu.Lock()
	tx, err := s.db.Begin()
	if err != nil {
		s.writeMu.Unlock()
		return 0, err
	}
	var deleted int64
	for _, table := range []string{"kpi_samples", "kpi_anomaly", "kpi_detect_run"} {
		res, err := tx.Exec(`DELETE FROM `+table+` WHERE ts < ?`, cutoff)
		if err != nil {
			_ = tx.Rollback()
			s.writeMu.Unlock()
			return deleted, err
		}
		if n, _ := res.RowsAffected(); n > 0 {
			deleted += n
		}
	}
	if err := tx.Commit(); err != nil {
		s.writeMu.Unlock()
		return deleted, err
	}
	s.clearSeriesCache()
	// VACUUM cannot run inside a transaction. Reclaim space after deletion.
	if _, err := s.db.Exec(`VACUUM`); err != nil {
		s.writeMu.Unlock()
		return deleted, err
	}
	s.writeMu.Unlock()
	return deleted, nil
}

// MarkRunsGlobalShift marks the given runs as global_shift without deleting
// their anomaly rows, so the console can still show what happened.
func (s *kpiStore) MarkRunsGlobalShift(business, node string, tsList []int64) error {
	if s == nil || s.db == nil || len(tsList) == 0 {
		return nil
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	stmt, err := tx.Prepare(`UPDATE kpi_detect_run SET global_shift = 1 WHERE business = ? AND node = ? AND ts = ?`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, ts := range tsList {
		if _, err := stmt.Exec(business, node, ts); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// RecountDetectRun recalculates anomalies/flagged for one run from kpi_anomaly.
func (s *kpiStore) RecountDetectRun(business, node string, ts int64) error {
	if s == nil || s.db == nil {
		return nil
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	var anomalies int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM kpi_anomaly WHERE business = ? AND node = ? AND ts = ?`,
		business, node, ts).Scan(&anomalies); err != nil {
		return err
	}
	var flagged int
	if err := s.db.QueryRow(`SELECT COUNT(DISTINCT device) FROM kpi_anomaly WHERE business = ? AND node = ? AND ts = ?`,
		business, node, ts).Scan(&flagged); err != nil {
		return err
	}
	_, err := s.db.Exec(`UPDATE kpi_detect_run SET anomalies = ?, flagged = ?, global_shift = 1 WHERE business = ? AND node = ? AND ts = ?`,
		anomalies, flagged, business, node, ts)
	return err
}

// AdmitAnomaliesInRange makes anomalies in [from, to] eligible for the reference window.
func (s *kpiStore) AdmitAnomaliesInRange(business, node string, from, to int64) error {
	if s == nil || s.db == nil {
		return nil
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	_, err := s.db.Exec(`UPDATE kpi_anomaly SET reference_admitted = 1
		WHERE business = ? AND node = ? AND ts >= ? AND ts <= ?`, business, node, from, to)
	return err
}

// RecentAnomalies returns recent anomaly rows for a business.
func (s *kpiStore) RecentAnomalies(business string, limit int) ([]kpiAnomaly, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("kpi store disabled")
	}
	if limit <= 0 {
		limit = 200
	}
	rows, err := s.db.Query(`SELECT business, node, device, ts, metric, kind, value, median, mad, z
		FROM kpi_anomaly WHERE business = ? ORDER BY ts DESC, device, metric LIMIT ?`, business, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]kpiAnomaly, 0)
	for rows.Next() {
		var a kpiAnomaly
		if err := rows.Scan(&a.Business, &a.Node, &a.Device, &a.Ts, &a.Metric, &a.Kind,
			&a.Value, &a.Median, &a.Mad, &a.Z); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func boolInt(v bool) int {
	if v {
		return 1
	}
	return 0
}
