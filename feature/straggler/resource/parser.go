package resource

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// =============================================================================
// Card Indexer (node-aware global IDs)
// =============================================================================

// cardIndexer assigns global integer card IDs to (node, cardID) pairs so that
// per-node card IDs (0-based within each node) can coexist in the shared
// map[int]float64 metric dicts without colliding. Flat (single-node) input
// keeps the raw card IDs as the global IDs for backward compatibility.
type cardIndexer struct {
	nodeCard map[string]map[int]int // node → local card ID → global ID
	nodeOf   map[int]string         // global → node
	localOf  map[int]int            // global → local (per-node) card ID
	nextID   int
}

func newCardIndexer() *cardIndexer {
	return &cardIndexer{
		nodeCard: map[string]map[int]int{},
		nodeOf:   map[int]string{},
		localOf:  map[int]int{},
	}
}

// globalID returns the global ID for (node, cardID), assigning a fresh one on
// first sight. Named nodes get sequential globals (0,1,2,...); the "none" node
// keeps the raw card ID so flat input behaves exactly as before.
func (ci *cardIndexer) globalID(node string, cardID int) int {
	if m, ok := ci.nodeCard[node]; ok {
		if g, ok := m[cardID]; ok {
			return g
		}
	} else {
		ci.nodeCard[node] = map[int]int{}
	}
	var g int
	if node == noneNode {
		g = cardID // flat: preserve raw IDs
		for ci.occupied(g) {
			g++
		}
	} else {
		g = ci.nextID
		for ci.occupied(g) {
			g++
		}
		ci.nextID = g + 1
	}
	ci.nodeCard[node][cardID] = g
	ci.nodeOf[g] = node
	ci.localOf[g] = cardID
	return g
}

func (ci *cardIndexer) occupied(g int) bool {
	_, ok := ci.nodeOf[g]
	return ok
}

func (ci *cardIndexer) sortedGlobalIDs() []int {
	ids := make([]int, 0, len(ci.nodeOf))
	for g := range ci.nodeOf {
		ids = append(ids, g)
	}
	sort.Ints(ids)
	return ids
}

func (ci *cardIndexer) nodeMap() map[int]string { return ci.nodeOf }
func (ci *cardIndexer) localMap() map[int]int   { return ci.localOf }

// =============================================================================
// CSV Parsing
// =============================================================================

// ParseCSV reads a KPI CSV file and returns the parsed time-series data.
//
// The CSV format is:
//   timestamp,NPU_CARD_POWER,NPU_CARD_TEMP,...,CPU_average
//   1784547926,"{""0"":1628,...}","{""0"":47,...}",...,"{""cpu1"":""4.26"",...}"
//
// Metric cells may be flat {cardID: value} (single node "none") or nested
// {nodeName: {cardID: value}} (multi-node).
func ParseCSV(filePath string) (*TimeSeriesData, error) {
	f, err := os.Open(filePath)
	if err != nil {
		return nil, fmt.Errorf("cannot open CSV %s: %w", filePath, err)
	}
	defer f.Close()

	reader := csv.NewReader(f)
	reader.LazyQuotes = true
	reader.TrimLeadingSpace = true

	records, err := reader.ReadAll()
	if err != nil {
		return nil, fmt.Errorf("cannot read CSV %s: %w", filePath, err)
	}
	if len(records) < 2 {
		return nil, fmt.Errorf("CSV %s has no data rows (only header)", filePath)
	}

	// Header row: identify column indices by name.
	header := records[0]
	colIdx, err := buildColumnIndex(header)
	if err != nil {
		return nil, fmt.Errorf("CSV %s: %w", filePath, err)
	}

	// Parse data rows. One indexer spans the whole file so a (node, cardID)
	// pair always maps to the same global ID across rows and metrics.
	idx := newCardIndexer()
	rows := make([]CSVRow, 0, len(records)-1)

	for i := 1; i < len(records); i++ {
		rec := records[i]
		row, err := parseRow(rec, colIdx, idx)
		if err != nil {
			fmt.Fprintf(os.Stderr, "[SLOWNODE ALGO] [WARN] skipping row %d in %s: %v\n", i, filePath, err)
			continue
		}
		rows = append(rows, row)
	}

	if len(rows) == 0 {
		return nil, fmt.Errorf("CSV %s: no valid data rows parsed", filePath)
	}

	// Sort rows by timestamp.
	sort.Slice(rows, func(i, j int) bool { return rows[i].Timestamp < rows[j].Timestamp })

	return &TimeSeriesData{
		Rows:    rows,
		RawRows: rows, // keep raw for counter metric processing
		CardIDs: idx.sortedGlobalIDs(),
		NodeOf:  idx.nodeMap(),
		LocalID: idx.localMap(),
	}, nil
}

// =============================================================================
// KPI Directory Parsing (multiple per-node CSV files + node_config.json)
// =============================================================================

// NodeCSVConfig maps one CSV file in a KPI directory to a physical node and the
// per-node cards (0-based) that are used.
type NodeCSVConfig struct {
	Node  string `json:"node"`
	Cards []int  `json:"cards"`
}

// ParseKPIDir reads a KPI directory: multiple per-node CSV files plus a fixed
// node_config.json describing, for each CSV file, the node it belongs to and
// the cards used. Returns a merged TimeSeriesData with node-aware global card
// IDs (via cardIndexer). Basic validation: every CSV has a config entry, every
// config CSV exists, and configured cards have data (warn otherwise).
func ParseKPIDir(dir string) (*TimeSeriesData, error) {
	cfgBytes, err := os.ReadFile(filepath.Join(dir, "node_config.json"))
	if err != nil {
		return nil, fmt.Errorf("read node_config.json in %s: %w", dir, err)
	}
	var nodeConfig map[string]NodeCSVConfig
	if err := json.Unmarshal(cfgBytes, &nodeConfig); err != nil {
		return nil, fmt.Errorf("parse node_config.json: %w", err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read dir %s: %w", dir, err)
	}
	var csvFiles []string
	csvSet := make(map[string]bool)
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".csv") {
			continue
		}
		csvFiles = append(csvFiles, filepath.Join(dir, e.Name()))
		csvSet[e.Name()] = true
	}
	if len(csvFiles) == 0 {
		return nil, fmt.Errorf("no CSV files in %s", dir)
	}

	// Basic validation: every CSV has a config entry; every config CSV exists.
	for _, f := range csvFiles {
		if _, ok := nodeConfig[filepath.Base(f)]; !ok {
			return nil, fmt.Errorf("node_config.json missing entry for %s", filepath.Base(f))
		}
	}
	for name := range nodeConfig {
		if !csvSet[name] {
			return nil, fmt.Errorf("node_config.json references missing CSV %s", name)
		}
	}

	idx := newCardIndexer()
	var rows []CSVRow
	for _, f := range csvFiles {
		cfg := nodeConfig[filepath.Base(f)]
		fileRows, err := parseCSVWithNode(f, cfg.Node, cfg.Cards, idx)
		if err != nil {
			return nil, err
		}
		rows = append(rows, fileRows...)
	}

	sort.Slice(rows, func(i, j int) bool { return rows[i].Timestamp < rows[j].Timestamp })

	return &TimeSeriesData{
		Rows:    rows,
		RawRows: rows,
		CardIDs: idx.sortedGlobalIDs(),
		NodeOf:  idx.nodeMap(),
		LocalID: idx.localMap(),
	}, nil
}

// parseCSVWithNode parses one per-node CSV: flat cells only, cards filtered to
// the node's configured set, and each card mapped to a global ID under the node
// name. Configured cards with no data are reported as warnings.
func parseCSVWithNode(csvPath, node string, cards []int, idx *cardIndexer) ([]CSVRow, error) {
	allowed := make(map[int]bool, len(cards))
	for _, c := range cards {
		allowed[c] = true
	}

	f, err := os.Open(csvPath)
	if err != nil {
		return nil, fmt.Errorf("open CSV %s: %w", csvPath, err)
	}
	defer f.Close()

	reader := csv.NewReader(f)
	reader.LazyQuotes = true
	reader.TrimLeadingSpace = true
	records, err := reader.ReadAll()
	if err != nil {
		return nil, fmt.Errorf("read CSV %s: %w", csvPath, err)
	}
	if len(records) < 2 {
		return nil, fmt.Errorf("CSV %s has no data rows", csvPath)
	}

	colIdx, err := buildColumnIndex(records[0])
	if err != nil {
		return nil, fmt.Errorf("CSV %s: %w", csvPath, err)
	}

	var rows []CSVRow
	for i := 1; i < len(records); i++ {
		rec := records[i]
		if colIdx.timestamp >= len(rec) || rec[colIdx.timestamp] == "" {
			continue
		}
		ts, perr := strconv.ParseInt(strings.TrimSpace(rec[colIdx.timestamp]), 10, 64)
		if perr != nil {
			continue
		}
		row := CSVRow{Timestamp: ts}
		row.Power = parseFlatMetricWithNode(rec, colIdx.power, "NPU_CARD_POWER", idx, node, allowed)
		row.Temp = parseFlatMetricWithNode(rec, colIdx.temp, "NPU_CARD_TEMP", idx, node, allowed)
		row.AICoreFreq = parseFlatMetricWithNode(rec, colIdx.aicoreFreq, "NPU_CARD_AICORE_FREQ", idx, node, allowed)
		row.AICoreUtil = parseFlatMetricWithNode(rec, colIdx.aicoreUtil, "NPU_CARD_AICORE_UTIL", idx, node, allowed)
		row.HBMBandwidthUtil = parseFlatMetricWithNode(rec, colIdx.hbmBandwidthUtil, "NPU_CARD_HBM_BANDWIDTH_UTIL", idx, node, allowed)
		row.HBMUtil = parseFlatMetricWithNode(rec, colIdx.hbmUtil, "NPU_CARD_HBM_UTIL", idx, node, allowed)
		row.TXBandwidth = parseFlatMetricWithNode(rec, colIdx.txBandwidth, "NPU_TX_BANDWIDTH", idx, node, allowed)
		row.RXPfcPkt = parseFlatMetricWithNode(rec, colIdx.rxPfcPkt, "NPU_RX_PFC_PKT", idx, node, allowed)
		row.RocETxErrPkt = parseFlatMetricWithNode(rec, colIdx.roceTxErrPkt, "NPU_ROCE_TX_ERR_PKT", idx, node, allowed)
		row.RocEOutOfOrder = parseFlatMetricWithNode(rec, colIdx.roceOutOfOrder, "NPU_ROCE_OUT_OF_ORDER", idx, node, allowed)
		row.RocENewPktRty = parseFlatMetricWithNode(rec, colIdx.roceNewPktRty, "NPU_ROCE_NEW_PKT_RTY", idx, node, allowed)
		row.NICRxAllPkg = parseFlatMetricWithNode(rec, colIdx.nicRxAllPkg, "NPU_NIC_RX_ALL_PKG", idx, node, allowed)
		row.CPUAvg = parseCPUJSON(rec, colIdx.cpuAvg)
		rows = append(rows, row)
	}

	// Warn for configured cards with no data anywhere in this CSV.
	seenLocal := make(map[int]bool)
	for _, row := range rows {
		for _, dict := range []map[int]float64{row.Power, row.Temp, row.AICoreFreq, row.AICoreUtil,
			row.HBMBandwidthUtil, row.HBMUtil, row.TXBandwidth, row.RXPfcPkt,
			row.RocETxErrPkt, row.RocEOutOfOrder, row.RocENewPktRty, row.NICRxAllPkg} {
			for g := range dict {
				seenLocal[idx.localMap()[g]] = true
			}
		}
	}
	for _, c := range cards {
		if !seenLocal[c] {
			fmt.Fprintf(os.Stderr, "[SLOWNODE ALGO] [WARN] %s: card %d in node_config has no data\n",
				filepath.Base(csvPath), c)
		}
	}

	return rows, nil
}

// parseFlatMetricWithNode parses a flat {cardID: value} metric cell, keeping
// only cards in `allowed` and mapping them to global IDs under `node`.
func parseFlatMetricWithNode(rec []string, idx int, name string, ci *cardIndexer, node string, allowed map[int]bool) map[int]float64 {
	if idx < 0 || idx >= len(rec) || rec[idx] == "" {
		return nil
	}
	raw := strings.ReplaceAll(rec[idx], `""`, `"`)
	var top map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &top); err != nil {
		fmt.Fprintf(os.Stderr, "[SLOWNODE ALGO] [WARN] cannot parse %s JSON: %v (raw: %.80s...)\n", name, err, raw)
		return nil
	}
	result := make(map[int]float64)
	for k, rawVal := range top {
		v, ok := parseJSONNumber(rawVal)
		if !ok {
			continue
		}
		c, aerr := strconv.Atoi(k)
		if aerr != nil {
			continue
		}
		if !allowed[c] {
			continue
		}
		result[ci.globalID(node, c)] = v
	}
	return result
}

// =============================================================================
// Column Index Mapping
// =============================================================================

type columnIndex struct {
	timestamp     int
	power         int
	temp          int
	aicoreFreq    int
	aicoreUtil    int
	hbmBandwidthUtil       int
	hbmUtil                int
	txBandwidth   int
	rxPfcPkt      int
	roceTxErrPkt  int
	roceOutOfOrder int
	roceNewPktRty int
	nicRxAllPkg   int
	cpuAvg        int
}

func buildColumnIndex(header []string) (columnIndex, error) {
	ci := columnIndex{
		timestamp:     -1,
		power:         -1,
		temp:          -1,
		aicoreFreq:    -1,
		aicoreUtil:    -1,
		hbmBandwidthUtil:       -1,
		hbmUtil:                -1,
		txBandwidth:   -1,
		rxPfcPkt:      -1,
		roceTxErrPkt:  -1,
		roceOutOfOrder: -1,
		roceNewPktRty: -1,
		nicRxAllPkg:   -1,
		cpuAvg:        -1,
	}

	nameToIdx := map[string]*int{
		"timestamp":              &ci.timestamp,
		"NPU_CARD_POWER":         &ci.power,
		"NPU_CARD_TEMP":          &ci.temp,
		"NPU_CARD_AICORE_FREQ":   &ci.aicoreFreq,
		"NPU_CARD_AICORE_UTIL":   &ci.aicoreUtil,
		"NPU_CARD_HBM_BANDWIDTH_UTIL":      &ci.hbmBandwidthUtil,
		"NPU_CARD_HBM_UTIL":                &ci.hbmUtil,
		"NPU_TX_BANDWIDTH":       &ci.txBandwidth,
		"NPU_RX_PFC_PKT":         &ci.rxPfcPkt,
		"NPU_ROCE_TX_ERR_PKT":    &ci.roceTxErrPkt,
		"NPU_ROCE_OUT_OF_ORDER":  &ci.roceOutOfOrder,
		"NPU_ROCE_NEW_PKT_RTY":   &ci.roceNewPktRty,
		"NPU_NIC_RX_ALL_PKG":     &ci.nicRxAllPkg,
		"CPU_average":            &ci.cpuAvg,
	}

	for i, h := range header {
		h = strings.TrimSpace(h)
		if ptr, ok := nameToIdx[h]; ok {
			*ptr = i
		}
	}

	// Only timestamp is strictly required; metrics use column-order fallback.
	if ci.timestamp < 0 {
		return ci, fmt.Errorf("missing required column: timestamp")
	}

	// Warn about missing metric columns (non-fatal).
	for name, ptr := range nameToIdx {
		if name != "timestamp" && *ptr < 0 {
			fmt.Fprintf(os.Stderr, "[SLOWNODE ALGO] [WARN] column '%s' not found in CSV header, will be empty\n", name)
		}
	}

	return ci, nil
}

// =============================================================================
// Row Parsing
// =============================================================================

func parseRow(rec []string, ci columnIndex, idx *cardIndexer) (CSVRow, error) {
	row := CSVRow{}

	// Timestamp.
	if ci.timestamp >= len(rec) || rec[ci.timestamp] == "" {
		return row, fmt.Errorf("missing timestamp")
	}
	ts, err := strconv.ParseInt(strings.TrimSpace(rec[ci.timestamp]), 10, 64)
	if err != nil {
		return row, fmt.Errorf("invalid timestamp %q: %w", rec[ci.timestamp], err)
	}
	row.Timestamp = ts

	// Parse each metric column as JSON dict (flat or node-nested).
	row.Power = parseMetricJSON(rec, ci.power, "NPU_CARD_POWER", idx)
	row.Temp = parseMetricJSON(rec, ci.temp, "NPU_CARD_TEMP", idx)
	row.AICoreFreq = parseMetricJSON(rec, ci.aicoreFreq, "NPU_CARD_AICORE_FREQ", idx)
	row.AICoreUtil = parseMetricJSON(rec, ci.aicoreUtil, "NPU_CARD_AICORE_UTIL", idx)
	row.HBMBandwidthUtil = parseMetricJSON(rec, ci.hbmBandwidthUtil, "NPU_CARD_HBM_BANDWIDTH_UTIL", idx)
	row.HBMUtil = parseMetricJSON(rec, ci.hbmUtil, "NPU_CARD_HBM_UTIL", idx)
	row.TXBandwidth = parseMetricJSON(rec, ci.txBandwidth, "NPU_TX_BANDWIDTH", idx)
	row.RXPfcPkt = parseMetricJSON(rec, ci.rxPfcPkt, "NPU_RX_PFC_PKT", idx)
	row.RocETxErrPkt = parseMetricJSON(rec, ci.roceTxErrPkt, "NPU_ROCE_TX_ERR_PKT", idx)
	row.RocEOutOfOrder = parseMetricJSON(rec, ci.roceOutOfOrder, "NPU_ROCE_OUT_OF_ORDER", idx)
	row.RocENewPktRty = parseMetricJSON(rec, ci.roceNewPktRty, "NPU_ROCE_NEW_PKT_RTY", idx)
	row.NICRxAllPkg = parseMetricJSON(rec, ci.nicRxAllPkg, "NPU_NIC_RX_ALL_PKG", idx)

	// CPU_average has string values (e.g. "4.26") so parse separately.
	row.CPUAvg = parseCPUJSON(rec, ci.cpuAvg)

	return row, nil
}

// parseMetricJSON parses a metric column into map[int]float64 keyed by GLOBAL
// card ID. The cell may be flat {cardID: value} (single node "none") or nested
// {nodeName: {cardID: value}} (multi-node). The indexer keeps (node, cardID)
// → global ID consistent across rows and metrics.
func parseMetricJSON(rec []string, idx int, name string, ci *cardIndexer) map[int]float64 {
	if idx < 0 || idx >= len(rec) || rec[idx] == "" {
		return nil
	}
	// The CSV quoting may double-quote the JSON. Un-double if needed.
	// e.g. "{""0"":1628}" → {"0":1628}
	raw := strings.ReplaceAll(rec[idx], `""`, `"`)

	var top map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &top); err != nil {
		fmt.Fprintf(os.Stderr, "[SLOWNODE ALGO] [WARN] cannot parse %s JSON: %v (raw: %.80s...)\n", name, err, raw)
		return nil
	}

	result := make(map[int]float64, len(top))
	for k, rawVal := range top {
		if isJSONObject(rawVal) {
			// Nested: {node: {cardID: value}}
			var inner map[string]json.RawMessage
			if err := json.Unmarshal(rawVal, &inner); err != nil {
				continue
			}
			for ck, cv := range inner {
				v, ok := parseJSONNumber(cv)
				if !ok {
					continue
				}
				c, aerr := strconv.Atoi(ck)
				if aerr != nil {
					continue
				}
				result[ci.globalID(k, c)] = v
			}
		} else {
			// Flat: {cardID: value}
			v, ok := parseJSONNumber(rawVal)
			if !ok {
				continue
			}
			c, aerr := strconv.Atoi(k)
			if aerr != nil {
				continue
			}
			result[ci.globalID(noneNode, c)] = v
		}
	}
	return result
}

// isJSONObject reports whether raw is a JSON object ({...}).
func isJSONObject(raw json.RawMessage) bool {
	raw = trimSpace(raw)
	return len(raw) > 0 && raw[0] == '{'
}

// parseJSONNumber extracts a numeric value from a JSON raw value, accepting a
// JSON number or a JSON string that parses as a number.
func parseJSONNumber(raw json.RawMessage) (float64, bool) {
	var num float64
	if err := json.Unmarshal(raw, &num); err == nil {
		return num, true
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		if f, err := strconv.ParseFloat(s, 64); err == nil {
			return f, true
		}
	}
	return 0, false
}

func trimSpace(raw json.RawMessage) json.RawMessage {
	s := strings.TrimSpace(string(raw))
	return json.RawMessage(s)
}

// parseCPUJSON parses CPU_average column: {"cpu1":"4.26","cpu2":"3.41",...}.
func parseCPUJSON(rec []string, idx int) map[string]string {
	if idx < 0 || idx >= len(rec) || rec[idx] == "" {
		return nil
	}
	raw := rec[idx]
	raw = strings.ReplaceAll(raw, `""`, `"`)

	var rawMap map[string]interface{}
	if err := json.Unmarshal([]byte(raw), &rawMap); err != nil {
		return nil
	}

	result := make(map[string]string, len(rawMap))
	for k, v := range rawMap {
		switch s := v.(type) {
		case string:
			result[k] = s
		case float64:
			result[k] = strconv.FormatFloat(s, 'f', -1, 64)
		}
	}
	return result
}

// =============================================================================
// Metric Value Accessors
// =============================================================================

// getMetricValues returns the values for a given metric from a CSVRow for the specified cards.
func getMetricValues(row CSVRow, metric MetricName, cardIDs []int) []float64 {
	dict := getMetricDict(row, metric)
	if dict == nil {
		return nil
	}
	vals := make([]float64, len(cardIDs))
	for i, cid := range cardIDs {
		if v, ok := dict[cid]; ok {
			vals[i] = v
		} else {
			vals[i] = 0 // missing = 0 (will be filtered by caller)
		}
	}
	return vals
}

// getMetricDict returns the raw map for a metric from a row.
func getMetricDict(row CSVRow, metric MetricName) map[int]float64 {
	switch metric {
	case MetricTemp:
		return row.Temp
	case MetricPower:
		return row.Power
	case MetricAICoreFreq:
		return row.AICoreFreq
	case MetricAICoreUtil:
		return row.AICoreUtil
	case MetricHBMBandwidthUtil:
		return row.HBMBandwidthUtil
	case MetricHBMUtil:
		return row.HBMUtil
	case MetricTXBandwidth:
		return row.TXBandwidth
	case MetricRXPfcPkt:
		return row.RXPfcPkt
	case MetricRocETxErrPkt:
		return row.RocETxErrPkt
	case MetricRocEOutOfOrder:
		return row.RocEOutOfOrder
	case MetricRocENewPktRty:
		return row.RocENewPktRty
	default:
		return nil
	}
}

// setMetricDict sets the value for a metric in a row (used by aggregator).
func setMetricDict(row *CSVRow, metric MetricName, vals map[int]float64) {
	switch metric {
	case MetricTemp:
		row.Temp = vals
	case MetricPower:
		row.Power = vals
	case MetricAICoreFreq:
		row.AICoreFreq = vals
	case MetricAICoreUtil:
		row.AICoreUtil = vals
	case MetricHBMBandwidthUtil:
		row.HBMBandwidthUtil = vals
	case MetricHBMUtil:
		row.HBMUtil = vals
	case MetricTXBandwidth:
		row.TXBandwidth = vals
	case MetricRXPfcPkt:
		row.RXPfcPkt = vals
	case MetricRocETxErrPkt:
		row.RocETxErrPkt = vals
	case MetricRocEOutOfOrder:
		row.RocEOutOfOrder = vals
	case MetricRocENewPktRty:
		row.RocENewPktRty = vals
	case MetricNICRxAllPkg:
		row.NICRxAllPkg = vals
	}
}

// getMetricDictFromRaw is like getMetricDict but uses RawRows for counter metric processing.
// MetricNICRxAllPkg is added for completeness.
const MetricNICRxAllPkg MetricName = "nic_rx_all_pkg"
