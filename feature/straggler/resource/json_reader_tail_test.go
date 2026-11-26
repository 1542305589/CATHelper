package resource

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeKPITailFile(t *testing.T, dir, name string, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

func kpiTailSampleLine(ts int, card int, power float64) string {
	return fmt.Sprintf(`{"ts":%d,"vals":{"%d":{"power":%v}}}`, ts, card, power)
}

// The tail reader must keep only complete lines from the tail, dropping the
// partial first and last lines produced by a byte-offset read.
func TestReadKPIFilesTailKeepsCompleteTailOnly(t *testing.T) {
	dir := t.TempDir()
	line1 := kpiTailSampleLine(1, 0, 100)
	line2 := kpiTailSampleLine(2, 0, 101)
	line3 := kpiTailSampleLine(3, 0, 102)
	content := line1 + "\n" + line2 + "\n" + line3 + "\n"
	writeKPITailFile(t, dir, "straggler_kpi_2026-01-01.jsonl", content)

	// Start a few bytes before the end of line1: line1 is partial and dropped.
	tailBytes := int64(len(line2) + len(line3) + 2 + 4)
	ts, err := ReadKPIFilesTail(dir, tailBytes)
	if err != nil {
		t.Fatalf("ReadKPIFilesTail: %v", err)
	}
	if len(ts.Rows) != 2 {
		t.Fatalf("rows = %d, want 2 (line1 tail dropped): %+v", len(ts.Rows), ts.Rows)
	}
	if ts.Rows[0].Timestamp != 2 || ts.Rows[1].Timestamp != 3 {
		t.Fatalf("timestamps = [%d %d], want [2 3]", ts.Rows[0].Timestamp, ts.Rows[1].Timestamp)
	}
	if len(ts.CardIDs) != 1 || ts.CardIDs[0] != 0 {
		t.Fatalf("card IDs = %v, want [0]", ts.CardIDs)
	}
}

func TestReadKPIFilesTailDropsTrailingPartialLine(t *testing.T) {
	dir := t.TempDir()
	line1 := kpiTailSampleLine(1, 0, 100)
	line2 := kpiTailSampleLine(2, 0, 101)
	partial := `{"ts":3,"vals":{"0":{"power":`
	writeKPITailFile(t, dir, "straggler_kpi_2026-01-01.jsonl", line1+"\n"+line2+"\n"+partial)

	ts, err := ReadKPIFilesTail(dir, DefaultKPITailBytes)
	if err != nil {
		t.Fatalf("ReadKPIFilesTail: %v", err)
	}
	if len(ts.Rows) != 2 {
		t.Fatalf("rows = %d, want 2 (trailing partial dropped): %+v", len(ts.Rows), ts.Rows)
	}
	if ts.Rows[0].Timestamp != 1 || ts.Rows[1].Timestamp != 2 {
		t.Fatalf("timestamps = [%d %d], want [1 2]", ts.Rows[0].Timestamp, ts.Rows[1].Timestamp)
	}
}

func TestReadKPIFilesTailSkipsEmptyAndBadLines(t *testing.T) {
	dir := t.TempDir()
	line1 := kpiTailSampleLine(1, 0, 100)
	line2 := kpiTailSampleLine(2, 0, 101)
	writeKPITailFile(t, dir, "straggler_kpi_2026-01-01.jsonl", line1+"\n\n{bad json}\n"+line2+"\n")

	ts, err := ReadKPIFilesTail(dir, DefaultKPITailBytes)
	if err != nil {
		t.Fatalf("ReadKPIFilesTail: %v", err)
	}
	if len(ts.Rows) != 2 {
		t.Fatalf("rows = %d, want 2 (bad/empty lines skipped): %+v", len(ts.Rows), ts.Rows)
	}
}

// A cpu_avg-only newest file must not produce card data; the reader falls back
// to the previous file's tail for real samples.
func TestReadKPIFilesTailFallsBackFromCPUAvgOnly(t *testing.T) {
	dir := t.TempDir()
	writeKPITailFile(t, dir, "straggler_kpi_2026-01-01.jsonl", kpiTailSampleLine(10, 0, 100)+"\n")
	writeKPITailFile(t, dir, "straggler_kpi_2026-01-02.jsonl", `{"ts":11,"cpu_avg":{"cpu0":"5"}}`+"\n")

	ts, err := ReadKPIFilesTail(dir, DefaultKPITailBytes)
	if err != nil {
		t.Fatalf("ReadKPIFilesTail: %v", err)
	}
	if len(ts.Rows) != 1 || ts.Rows[0].Timestamp != 10 {
		t.Fatalf("rows = %+v, want the previous file's ts=10 sample", ts.Rows)
	}
	if len(ts.CardIDs) != 1 || ts.CardIDs[0] != 0 {
		t.Fatalf("card IDs = %v, want [0]", ts.CardIDs)
	}
	if ts.Rows[0].Power[0] != 100 {
		t.Fatalf("power = %v, want 100", ts.Rows[0].Power)
	}
}

func TestReadKPIFilesTailNoSamplesError(t *testing.T) {
	dir := t.TempDir()
	writeKPITailFile(t, dir, "straggler_kpi_2026-01-01.jsonl", `{"ts":1,"cpu_avg":{"cpu0":"5"}}`+"\n")
	writeKPITailFile(t, dir, "straggler_kpi_2026-01-02.jsonl", `{"ts":2,"cpu_avg":{"cpu0":"6"}}`+"\n")

	_, err := ReadKPIFilesTail(dir, DefaultKPITailBytes)
	if err == nil || !strings.Contains(err.Error(), "no KPI samples") {
		t.Fatalf("error = %v, want no KPI samples", err)
	}
}

// node_config.json uses per-node subdirectories and an allowed-card filter.
func TestReadKPIFilesTailNodeConfig(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "n1"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "node_config.json"),
		[]byte(`{"n1":{"node":"node-a","cards":[0,1]}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	line := `{"ts":1,"vals":{"0":{"power":100},"1":{"power":101},"2":{"power":999}}}`
	writeKPITailFile(t, filepath.Join(dir, "n1"), "straggler_kpi_2026-01-01.jsonl", line+"\n")

	ts, err := ReadKPIFilesTail(dir, DefaultKPITailBytes)
	if err != nil {
		t.Fatalf("ReadKPIFilesTail: %v", err)
	}
	if len(ts.CardIDs) != 2 {
		t.Fatalf("card IDs = %v, want 2 allowed cards", ts.CardIDs)
	}
	for _, id := range ts.CardIDs {
		if ts.NodeOf[id] != "node-a" {
			t.Fatalf("card %d node = %q, want node-a", id, ts.NodeOf[id])
		}
	}
	if _, ok := ts.Rows[0].Power[ts.CardIDs[0]]; !ok {
		t.Fatalf("allowed cards must have power values: %+v", ts.Rows[0].Power)
	}
}
