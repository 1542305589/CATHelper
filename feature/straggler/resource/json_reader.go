package resource

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// KPISample mirrors the JSONL record CATMonitor's stragglerout module writes
// (features/stragglerout/sample.go). One record = one timestamp's per-card KPI
// values, 1:1 with this package's CSVRow so the JSON reader can feed the same
// detection pipeline the CSV parser does.
//
// Vals may be flat {cardID: {field: value}} (single node "none") or nested
// {nodeName: {cardID: {field: value}}} (multi-node). RawMessage preserves both
// shapes until sampleToRow sniffs them.
type KPISample struct {
	Timestamp int64                         `json:"ts"`
	Vals      map[string]json.RawMessage    `json:"vals,omitempty"`
	CPUAvg    map[string]string             `json:"cpu_avg,omitempty"` // cpuName -> util%
}

// ReadKPIFiles reads every straggler_kpi_{date}.jsonl file in dir and
// reconstructs the same *TimeSeriesData ParseCSV would produce. Rows are sorted
// by timestamp; CardIDs/NodeOf/LocalID come from one indexer spanning all files.
//
// Field names in the JSONL are the straggler KPI field names (temp, power,
// aicore_freq, aicore_util, hbm_bandwidth_util, hbm_util, tx_bandwidth,
// rx_pfc_pkt, roce_tx_err_pkt, roce_out_of_order, roce_new_pkt_rty), mapped
// back onto the CSVRow metric dicts.
func ReadKPIFiles(dir string) (*TimeSeriesData, error) {
	idx := newCardIndexer()
	var rows []CSVRow

	// Optional node_config.json switches the layout to multi-node: each node has
	// its own subdirectory (folder name → {node, cards used}), mirroring the
	// --kpi-path node_config.json. Without it, files are read directly from dir
	// (single node "none", or nested multi-node inside each sample).
	cfg, hasCfg, err := readJSONLNodeConfig(dir)
	if err != nil {
		return nil, err
	}

	if hasCfg {
		for folder, nc := range cfg {
			if nc.Node == "" {
				return nil, fmt.Errorf("node_config.json: folder %q missing node name", folder)
			}
			if info, serr := os.Stat(filepath.Join(dir, folder)); serr != nil || !info.IsDir() {
				return nil, fmt.Errorf("node_config.json folder %q missing in %s", folder, dir)
			}
			allowed := make(map[int]bool, len(nc.Cards))
			for _, c := range nc.Cards {
				allowed[c] = true
			}
			for _, path := range listJSONLFiles(filepath.Join(dir, folder)) {
				fileRows, rerr := readKPIFile(path, idx, nc.Node, allowed)
				if rerr != nil {
					return nil, rerr
				}
				rows = append(rows, fileRows...)
			}
		}
	} else {
		for _, path := range listJSONLFiles(dir) {
			fileRows, rerr := readKPIFile(path, idx, "", nil)
			if rerr != nil {
				return nil, rerr
			}
			rows = append(rows, fileRows...)
		}
	}

	if len(rows) == 0 {
		return nil, fmt.Errorf("no KPI samples in %s", dir)
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

// readJSONLNodeConfig reads the optional node_config.json in a JSONL directory:
// folder (subdirectory) name → {node, cards used}. Returns (nil, false) when the
// file is absent, so callers keep the legacy single-directory behavior.
func readJSONLNodeConfig(dir string) (map[string]NodeCSVConfig, bool, error) {
	path := filepath.Join(dir, "node_config.json")
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("read node_config.json: %w", err)
	}
	var cfg map[string]NodeCSVConfig
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return nil, false, fmt.Errorf("parse node_config.json: %w", err)
	}
	return cfg, true, nil
}

// readKPIFile decodes one straggler_kpi_{date}.jsonl file into CSVRows.
// node is the explicit node name when reading a per-node subdirectory layout
// (node_config.json); empty means legacy sniffing. allowed filters per-node
// cards when non-nil.
func readKPIFile(path string, idx *cardIndexer, node string, allowed map[int]bool) ([]CSVRow, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil // missing date file is fine (no data that day)
		}
		return nil, fmt.Errorf("cannot open %s: %w", path, err)
	}
	defer f.Close()

	var rows []CSVRow
	scanner := bufio.NewScanner(f)
	// KPI samples can be long (many cards × 10 metrics); raise the buffer.
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var s KPISample
		if err := json.Unmarshal(line, &s); err != nil {
			// skip malformed line rather than aborting the whole read
			fmt.Fprintf(os.Stderr, "[SLOWNODE ALGO] [WARN] skipping bad kpi line in %s: %v\n", path, err)
			continue
		}
		rows = append(rows, sampleToRow(s, idx, node, allowed))
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	return rows, nil
}

// sampleToRow converts one KPISample to a CSVRow, mapping the straggler KPI
// field names back onto the CSVRow metric dicts (global card ID → value).
//
// With an explicit node (multi-node node_config layout) the whole sample belongs
// to one node: card keys are per-node and filtered to `allowed`; without it,
// Vals may be flat {cardID: {field: value}} (node "none") or nested
// {node: {cardID: {field: value}}} (multi-node) and are sniffed.
func sampleToRow(s KPISample, idx *cardIndexer, node string, allowed map[int]bool) CSVRow {
	row := CSVRow{Timestamp: s.Timestamp, CPUAvg: s.CPUAvg}

	// Explicit node: per-node subdirectory layout, card keys are per-node.
	if node != "" {
		for cStr, rawCard := range s.Vals {
			c, err := strconv.Atoi(cStr)
			if err != nil {
				continue
			}
			if allowed != nil && !allowed[c] {
				continue
			}
			var cardFields map[string]json.RawMessage
			if err := json.Unmarshal(rawCard, &cardFields); err != nil {
				continue
			}
			g := idx.globalID(node, c)
			for field, fv := range cardFields {
				if v, ok := parseJSONNumber(fv); ok {
					assignMetricField(&row, g, field, v)
				}
			}
		}
		return row
	}

	for key, rawInner := range s.Vals {
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(rawInner, &fields); err != nil {
			continue
		}
		nested := false
		for _, fv := range fields {
			if isJSONObject(fv) {
				nested = true
			}
			break // sniff on the first field value
		}
		if !nested {
			// flat: key = cardID, fields = field → value
			c, err := strconv.Atoi(key)
			if err != nil {
				continue
			}
			g := idx.globalID(noneNode, c)
			for field, fv := range fields {
				if v, ok := parseJSONNumber(fv); ok {
					assignMetricField(&row, g, field, v)
				}
			}
			continue
		}
		// nested: key = node, fields = cardID → {field: value}
		for ck, cv := range fields {
			var cardFields map[string]json.RawMessage
			if err := json.Unmarshal(cv, &cardFields); err != nil {
				continue
			}
			c, err := strconv.Atoi(ck)
			if err != nil {
				continue
			}
			g := idx.globalID(key, c)
			for field, fv := range cardFields {
				if v, ok := parseJSONNumber(fv); ok {
					assignMetricField(&row, g, field, v)
				}
			}
		}
	}
	return row
}

// assignMetricField places a value into the right CSVRow metric dict by
// straggler KPI field name. Kept explicit (no reflection) for clarity.
func assignMetricField(row *CSVRow, cid int, field string, val float64) {
	switch field {
	case "temp":
		setOnce(&row.Temp, cid, val)
	case "power":
		setOnce(&row.Power, cid, val)
	case "aicore_freq":
		setOnce(&row.AICoreFreq, cid, val)
	case "aicore_util":
		setOnce(&row.AICoreUtil, cid, val)
	case "hbm_bandwidth_util":
		setOnce(&row.HBMBandwidthUtil, cid, val)
	case "hbm_util":
		setOnce(&row.HBMUtil, cid, val)
	case "tx_bandwidth":
		setOnce(&row.TXBandwidth, cid, val)
	case "rx_pfc_pkt":
		setOnce(&row.RXPfcPkt, cid, val)
	case "roce_tx_err_pkt":
		setOnce(&row.RocETxErrPkt, cid, val)
	case "roce_out_of_order":
		setOnce(&row.RocEOutOfOrder, cid, val)
	case "roce_new_pkt_rty":
		setOnce(&row.RocENewPktRty, cid, val)
	case "nic_rx_all_pkg":
		setOnce(&row.NICRxAllPkg, cid, val)
	}
}

// setOnce lazily allocates a metric dict and sets cid→val. If cid already has
// a value (e.g. two samples merged), the last write wins (caller order).
func setOnce(m *map[int]float64, cid int, val float64) {
	if *m == nil {
		*m = make(map[int]float64)
	}
	(*m)[cid] = val
}

// listJSONLFiles returns the sorted paths of all straggler_kpi_*.jsonl files
// directly inside dir. The whole history is read (there is no time-range window).
func listJSONLFiles(dir string) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var paths []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasPrefix(name, "straggler_kpi_") || !strings.HasSuffix(name, ".jsonl") {
			continue
		}
		paths = append(paths, filepath.Join(dir, name))
	}
	sort.Strings(paths)
	return paths
}
