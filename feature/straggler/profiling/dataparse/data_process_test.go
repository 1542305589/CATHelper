package dataparse

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The CSV header must match the Python skill exactly (level0 columns).
func TestWriteResultsToCSVHeaderParity(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "global_rank_0.csv")
	pm := PerformanceMetrics{
		StepIndex: 0, StepDuration: 100, ZPDevice: 10, ZPDuration: 20, ZPHost: 30,
		ZPCount: 0, ZPKernel: 40, MemcpyAsync: 50, KernelAivec: 60, HostDuration: 70,
		DataLoader: 80, MemcpyCount: 3, MemcpyLinked: 2, MemcpyExternal: 1,
		Durations: map[string]int{"tp": 11}, Counts: map[string]int{"tp": 5},
	}
	if err := WriteResultsToCSV(path, []PerformanceMetrics{pm}); err != nil {
		t.Fatalf("write csv: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read csv: %v", err)
	}
	lines := strings.Split(strings.TrimRight(string(raw), "\r\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("csv lines = %d, want 2\n%s", len(lines), raw)
	}
	got := strings.Split(lines[0], ",")
	want := []string{
		"StepIndex", "StepDuration", "ZP_Device", "ZP_Duration",
		"ZP_Host", "ZP_Count", "KERNEL_AICORE",
		"MEMCPY_ASYNC", "KERNEL_AIVEC", "HostDuration", "DataLoader",
		"tp_Duration", "tp_Count",
		"MEMCPY_ASYNC_Count", "MEMCPY_LINKED_Count", "MEMCPY_EXTERNAL_Count",
	}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("header = %v\nwant   = %v", got, want)
	}
}

// Python's calculate_mid_mean_pair is a PLAIN mean (not a trimmed mean).
func TestCalculateMeanPairPlainMean(t *testing.T) {
	d, c, err := CalculateMeanPair([]OpStat{{1, 1}, {2, 2}, {3, 3}, {100, 4}})
	if err != nil {
		t.Fatalf("CalculateMeanPair: %v", err)
	}
	if d != 27 { // (1+2+3+100)/4 = 26.5 -> 27
		t.Errorf("duration mean = %d, want 27", d)
	}
	if c != 3 { // (1+2+3+4)/4 = 2.5 -> 3
		t.Errorf("count mean = %d, want 3", c)
	}
}

// Host durations use CANN_API first, then MSTX_EVENTS, one entry per op.
func TestHostDurationsPrecedence(t *testing.T) {
	cann := map[int]HostOp{1: {StartNs: 10, EndNs: 25}, 2: {StartNs: 100, EndNs: 110}}
	mstx := map[int]HostOp{1: {StartNs: 50, EndNs: 90}, 3: {StartNs: 200, EndNs: 215}}
	got := hostDurationsFor([]int{1, 2, 3}, cann, mstx)
	want := []int{15, 10, 15}
	if len(got) != len(want) {
		t.Fatalf("durations = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("durations = %v, want %v", got, want)
		}
	}
}

// Only Python's level0 domain whitelist contributes *_Duration/_Count columns.
func TestDomainAllowedWhitelist(t *testing.T) {
	for _, x := range []string{"tp", "ep", "exp", "pp", "cp", "tp_exp", "dp_modulo_exp_cp", "embd", "mc2", "dp"} {
		if !domainAllowed(x) {
			t.Errorf("domainAllowed(%q) = false, want true", x)
		}
	}
	for _, x := range []string{"foo", "TP_UNKNOWN"} {
		if domainAllowed(x) {
			t.Errorf("domainAllowed(%q) = true, want false", x)
		}
	}
}
