package dataparse

import (
	"database/sql"
	"testing"

	_ "modernc.org/sqlite"
)

// Python pre-seeds every domain to 0 before its early returns and keeps the
// memcpy counters at -1 ("not available"), so a rank with no communication ops
// must still emit the domain columns and -1 counters.
func TestTimeDiffForStepEarlyReturnDefaults(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE STRING_IDS (value TEXT, id INTEGER)`); err != nil {
		t.Fatalf("create STRING_IDS: %v", err)
	}

	// Non-empty xp map whose group name is absent from STRING_IDS → Python's
	// "no group ids" early return.
	pm, err := TimeDiffForStep(db, map[string]string{"tp": "group_name_42"}, StepTime{ID: 0, StartNs: 0, EndNs: 1000})
	if err != nil {
		t.Fatalf("TimeDiffForStep: %v", err)
	}
	if pm.MemcpyLinked != -1 || pm.MemcpyExternal != -1 {
		t.Fatalf("memcpy counters = %d/%d, want -1/-1", pm.MemcpyLinked, pm.MemcpyExternal)
	}
	if _, ok := pm.Durations["tp"]; !ok {
		t.Fatalf("tp_Duration column missing: %v", pm.Durations)
	}
	if _, ok := pm.Counts["tp"]; !ok {
		t.Fatalf("tp_Count column missing: %v", pm.Counts)
	}
	if pm.ZPHost != invalidData {
		t.Fatalf("ZP_Host = %d, want %d (no host data)", pm.ZPHost, invalidData)
	}
}
