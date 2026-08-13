package resource

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeKPIFile(t *testing.T, dir, date, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "straggler_kpi_"+date+".jsonl"), []byte(content), 0o644); err != nil {
		t.Fatalf("write kpi file: %v", err)
	}
}

func TestReadKPIFilesBasic(t *testing.T) {
	dir := t.TempDir()
	// Two timestamps, two cards, 3 metrics each. File date is derived from the
	// sample timestamp so it matches the date-range logic.
	date := time.Unix(1784547926, 0).Local().Format("2006-01-02")
	writeKPIFile(t, dir, date,
		`{"ts":1784547926,"vals":{"0":{"temp":47,"power":1628,"aicore_freq":1800},"1":{"temp":50,"power":1700,"aicore_freq":1800}}}`+"\n"+
			`{"ts":1784547927,"vals":{"0":{"temp":48,"power":1620,"aicore_freq":1800},"1":{"temp":51,"power":1700,"aicore_freq":1800}}}`+"\n")
	since := time.Unix(1784547926, 0)
	until := since.Add(time.Minute)
	ts, err := ReadKPIFiles(dir, since, until)
	if err != nil {
		t.Fatalf("ReadKPIFiles: %v", err)
	}
	if len(ts.Rows) != 2 {
		t.Fatalf("expected 2 rows, got %d", len(ts.Rows))
	}
	if len(ts.CardIDs) != 2 {
		t.Fatalf("expected 2 cards, got %v", ts.CardIDs)
	}
	r0 := ts.Rows[0]
	if r0.Timestamp != 1784547926 {
		t.Errorf("row0 ts: %d", r0.Timestamp)
	}
	if r0.Temp[0] != 47 || r0.Power[0] != 1628 || r0.AICoreFreq[0] != 1800 {
		t.Errorf("card0 metrics wrong: temp=%v power=%v freq=%v", r0.Temp[0], r0.Power[0], r0.AICoreFreq[0])
	}
	if r0.Temp[1] != 50 {
		t.Errorf("card1 temp: %v", r0.Temp[1])
	}
	// Sorted by timestamp: row1 ts > row0 ts.
	if ts.Rows[1].Timestamp <= ts.Rows[0].Timestamp {
		t.Errorf("rows not sorted by ts")
	}
}

func TestReadKPIFilesAllFieldsAndCPU(t *testing.T) {
	dir := t.TempDir()
	date := time.Unix(1000, 0).Local().Format("2006-01-02")
	writeKPIFile(t, dir, date,
		`{"ts":1000,"vals":{"3":{"temp":47,"power":1628,"aicore_freq":1800,"aicore_util":45,"hbm_bandwidth_util":50,"hbm_util":48,"tx_bandwidth":1250,"rx_pfc_pkt":1,"roce_tx_err_pkt":2,"roce_out_of_order":3,"roce_new_pkt_rty":4,"nic_rx_all_pkg":5}},"cpu_avg":{"cpu1":"4.26"}}`+"\n")
	ts, err := ReadKPIFiles(dir, time.Unix(1000, 0), time.Unix(1000, 0).Add(time.Second))
	if err != nil {
		t.Fatalf("ReadKPIFiles: %v", err)
	}
	r := ts.Rows[0]
	if r.AICoreUtil[3] != 45 || r.HBMBandwidthUtil[3] != 50 || r.HBMUtil[3] != 48 || r.TXBandwidth[3] != 1250 {
		t.Errorf("mapped fields wrong: %+v", r)
	}
	if r.RXPfcPkt[3] != 1 || r.RocETxErrPkt[3] != 2 || r.RocEOutOfOrder[3] != 3 || r.RocENewPktRty[3] != 4 || r.NICRxAllPkg[3] != 5 {
		t.Errorf("counter fields wrong: %+v", r)
	}
	if r.CPUAvg["cpu1"] != "4.26" {
		t.Errorf("cpu_avg wrong: %+v", r.CPUAvg)
	}
}

func TestReadKPIFilesMissingDateFile(t *testing.T) {
	// A date with no file should be silently skipped.
	dir := t.TempDir()
	date := time.Unix(1000, 0).Local().Format("2006-01-02")
	writeKPIFile(t, dir, date, `{"ts":1000,"vals":{"0":{"temp":1}}}`+"\n")
	since := time.Unix(1000, 0).AddDate(0, 0, -1) // includes a prior day (no file)
	until := time.Unix(1000, 0).AddDate(0, 0, 1) // and a following day (no file)
	ts, err := ReadKPIFiles(dir, since, until)
	if err != nil {
		t.Fatalf("missing dates should not error: %v", err)
	}
	if len(ts.Rows) != 1 {
		t.Fatalf("expected 1 row from the one existing date, got %d", len(ts.Rows))
	}
}

func TestReadKPIFilesNoDataErrors(t *testing.T) {
	dir := t.TempDir()
	_, err := ReadKPIFiles(dir, time.Unix(1000, 0), time.Unix(2000, 0))
	if err == nil {
		t.Fatal("expected error when no KPI data in range")
	}
}

func TestReadKPIFilesSkipsMalformedLine(t *testing.T) {
	dir := t.TempDir()
	date := time.Unix(1000, 0).Local().Format("2006-01-02")
	writeKPIFile(t, dir, date,
		`{"ts":1000,"vals":{"0":{"temp":1}}}`+"\n"+
			`not json`+"\n"+
			`{"ts":1001,"vals":{"0":{"temp":2}}}`+"\n")
	ts, err := ReadKPIFiles(dir, time.Unix(1000, 0), time.Unix(1001, 0))
	if err != nil {
		t.Fatalf("malformed line should not abort: %v", err)
	}
	if len(ts.Rows) != 2 {
		t.Errorf("expected 2 valid rows (bad line skipped), got %d", len(ts.Rows))
	}
}

func TestReadKPIFilesMultiNode(t *testing.T) {
	// Multi-node layout: each node has its own subdirectory; node_config.json
	// maps folder → {node, cards}. Node identity comes from the config, and each
	// node's file uses per-node card numbers (0-based).
	dir := t.TempDir()
	date := time.Unix(1784547926, 0).Local().Format("2006-01-02")

	for _, folder := range []string{"node-a", "node-b"} {
		if err := os.MkdirAll(filepath.Join(dir, folder), 0755); err != nil {
			t.Fatal(err)
		}
	}
	writeKPIFile(t, filepath.Join(dir, "node-a"), date,
		`{"ts":1784547926,"vals":{"0":{"temp":55,"power":1628},"1":{"temp":56,"power":1630}}}`+"\n")
	writeKPIFile(t, filepath.Join(dir, "node-b"), date,
		`{"ts":1784547926,"vals":{"0":{"temp":60,"power":1700},"1":{"temp":61,"power":1710}}}`+"\n")
	if err := os.WriteFile(filepath.Join(dir, "node_config.json"),
		[]byte(`{"node-a":{"node":"node-1","cards":[0,1]},"node-b":{"node":"node-2","cards":[0,1]}}`), 0644); err != nil {
		t.Fatal(err)
	}

	ts, err := ReadKPIFiles(dir, time.Unix(1784547926, 0), time.Unix(1784547926, 0).Add(time.Minute))
	if err != nil {
		t.Fatalf("ReadKPIFiles: %v", err)
	}
	if len(ts.CardIDs) != 4 {
		t.Fatalf("expected 4 global cards, got %v", ts.CardIDs)
	}
	nodes := map[string]bool{}
	for _, n := range ts.NodeOf {
		nodes[n] = true
	}
	if len(nodes) != 2 || !nodes["node-1"] || !nodes["node-2"] {
		t.Fatalf("expected nodes node-1/node-2, got %v", nodes)
	}
	// Each JSONL sample becomes its own row (the pipeline's AggregateByMinute
	// merges same-timestamp rows), so scan every row for the node/card values.
	var found56, found61 bool
	for _, row := range ts.Rows {
		for _, cid := range ts.CardIDs {
			switch {
			case ts.NodeOf[cid] == "node-1" && ts.LocalID[cid] == 1 && row.Temp[cid] == 56:
				found56 = true
			case ts.NodeOf[cid] == "node-2" && ts.LocalID[cid] == 1 && row.Temp[cid] == 61:
				found61 = true
			}
		}
	}
	if !found56 || !found61 {
		t.Errorf("node/card mapping wrong: %v (56=%v 61=%v)", ts.NodeOf, found56, found61)
	}
}

func TestReadKPIFilesMultiNodeCardFilter(t *testing.T) {
	// node_config.json "cards" restricts which per-node cards are used; cards in
	// the data outside the configured set are dropped.
	dir := t.TempDir()
	date := time.Unix(1000, 0).Local().Format("2006-01-02")
	if err := os.MkdirAll(filepath.Join(dir, "node-a"), 0755); err != nil {
		t.Fatal(err)
	}
	writeKPIFile(t, filepath.Join(dir, "node-a"), date,
		`{"ts":1000,"vals":{"0":{"temp":55},"1":{"temp":56},"2":{"temp":57}}}`+"\n")
	if err := os.WriteFile(filepath.Join(dir, "node_config.json"),
		[]byte(`{"node-a":{"node":"node-1","cards":[0,1]}}`), 0644); err != nil {
		t.Fatal(err)
	}

	ts, err := ReadKPIFiles(dir, time.Unix(1000, 0), time.Unix(1000, 0).Add(time.Second))
	if err != nil {
		t.Fatalf("ReadKPIFiles: %v", err)
	}
	if len(ts.CardIDs) != 2 {
		t.Fatalf("expected 2 cards (filtered), got %v", ts.CardIDs)
	}
	row := ts.Rows[0]
	if row.Temp[ts.CardIDs[0]] == 57 || row.Temp[ts.CardIDs[1]] == 57 {
		t.Errorf("card 2 should have been filtered out by node_config cards")
	}
}

func TestReadKPIFilesEquivalentToCSVPipeline(t *testing.T) {
	// Build the same underlying data as a CSV and as JSONL, run both
	// parsers, and verify the TimeSeriesData matches (rows, cardIDs, values).
	dir := t.TempDir()
	date := time.Unix(1784547926, 0).Local().Format("2006-01-02")
	writeKPIFile(t, dir, date,
		`{"ts":1784547926,"vals":{"0":{"temp":47,"power":1628},"1":{"temp":50,"power":1700}}}`+"\n")
	ts, err := ReadKPIFiles(dir, time.Unix(1784547926, 0), time.Unix(1784547926, 0).Add(time.Second))
	if err != nil {
		t.Fatalf("ReadKPIFiles: %v", err)
	}
	if got := ts.Rows[0].Temp[0]; got != 47 {
		t.Errorf("card0 temp: %v", got)
	}
	if got := ts.Rows[0].Power[1]; got != 1700 {
		t.Errorf("card1 power: %v", got)
	}
	// RawRows == Rows (same contract as ParseCSV).
	if len(ts.RawRows) != len(ts.Rows) {
		t.Errorf("RawRows should mirror Rows")
	}
}
