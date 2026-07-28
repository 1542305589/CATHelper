package resource

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"time"
)

// KPISample mirrors the JSONL record CATMonitor's stragglerout module writes
// (features/stragglerout/sample.go). One record = one timestamp's per-card KPI
// values, 1:1 with this package's CSVRow so the JSON reader can feed the same
// detection pipeline the CSV parser does.
type KPISample struct {
	Timestamp int64                          `json:"ts"`
	Vals      map[string]map[string]float64  `json:"vals,omitempty"`    // cardID -> field -> value
	CPUAvg    map[string]string              `json:"cpu_avg,omitempty"`  // cpuName -> util%
}

// ReadKPIFiles reads all straggler_kpi_{date}.jsonl files in dir whose date
// falls in [since, until] (inclusive, by local date) and reconstructs the
// same *TimeSeriesData ParseCSV would produce. Rows are sorted by timestamp;
// CardIDs are the union of all cards seen.
//
// Field names in the JSONL are the straggler KPI field names (temp, power,
// aicore_freq, aicore_util, hbm_util, tx_bandwidth, rx_pfc_pkt,
// roce_tx_err_pkt, roce_out_of_order, roce_new_pkt_rty), mapped back onto the
// CSVRow metric dicts.
func ReadKPIFiles(dir string, since, until time.Time) (*TimeSeriesData, error) {
	dates := dateRange(since, until)
	var rows []CSVRow
	cardIDSet := make(map[int]bool)

	for _, date := range dates {
		path := filepath.Join(dir, "straggler_kpi_"+date+".jsonl")
		fileRows, err := readKPIFile(path)
		if err != nil {
			return nil, err
		}
		for _, r := range fileRows {
			rows = append(rows, r)
			for cid := range r.Power {
				cardIDSet[cid] = true
			}
			for cid := range r.Temp {
				cardIDSet[cid] = true
			}
		}
	}
	if len(rows) == 0 {
		return nil, fmt.Errorf("no KPI samples in %s for range [%s, %s]", dir, since.Format("2006-01-02"), until.Format("2006-01-02"))
	}

	sort.Slice(rows, func(i, j int) bool { return rows[i].Timestamp < rows[j].Timestamp })

	cardIDs := make([]int, 0, len(cardIDSet))
	for cid := range cardIDSet {
		cardIDs = append(cardIDs, cid)
	}
	sort.Ints(cardIDs)

	return &TimeSeriesData{
		Rows:    rows,
		RawRows: rows,
		CardIDs: cardIDs,
	}, nil
}

// readKPIFile decodes one straggler_kpi_{date}.jsonl file into CSVRows.
func readKPIFile(path string) ([]CSVRow, error) {
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
		rows = append(rows, sampleToRow(s))
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	return rows, nil
}

// sampleToRow converts one KPISample to a CSVRow, mapping the straggler KPI
// field names back onto the CSVRow metric dicts (cardID int → value).
func sampleToRow(s KPISample) CSVRow {
	row := CSVRow{Timestamp: s.Timestamp, CPUAvg: s.CPUAvg}
	for cidStr, fields := range s.Vals {
		cid, err := strconv.Atoi(cidStr)
		if err != nil {
			continue
		}
		for field, val := range fields {
			assignMetricField(&row, cid, field, val)
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

// dateRange returns the list of "YYYY-MM-DD" strings from since to until
// (inclusive), by local date. Tolerates until < since by swapping.
func dateRange(since, until time.Time) []string {
	if since.After(until) {
		since, until = until, since
	}
	var out []string
	d := since
	for !d.After(until) {
		out = append(out, d.Local().Format("2006-01-02"))
		d = d.AddDate(0, 0, 1)
	}
	return out
}
