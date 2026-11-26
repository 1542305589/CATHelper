package daemon

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func noopLogf(string, ...any) {}

func writeTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestCopyProfilerDataExcludesAndCounts(t *testing.T) {
	src := t.TempDir()
	writeTestFile(t, filepath.Join(src, "master_0", "a.db"), "abc")
	writeTestFile(t, filepath.Join(src, ".hidden"), "x")
	writeTestFile(t, filepath.Join(src, "op_metric", "ignore"), "ignored")
	writeTestFile(t, filepath.Join(src, "analysis_result", "ignore"), "ignored")

	symlinkCreated := false
	if err := os.Symlink(filepath.Join(src, "master_0", "a.db"), filepath.Join(src, "link.db")); err == nil {
		symlinkCreated = true
	} else {
		t.Logf("symlink not supported on this platform: %v", err)
	}

	dst := filepath.Join(t.TempDir(), "profiler_data")
	copied, source, err := copyProfilerData(src, dst, noopLogf)
	if err != nil {
		t.Fatalf("copyProfilerData: %v", err)
	}
	if copied != 4 || source != 4 {
		t.Fatalf("copied/source = %d/%d, want 4/4", copied, source)
	}
	if got, err := os.ReadFile(filepath.Join(dst, "master_0", "a.db")); err != nil || string(got) != "abc" {
		t.Fatalf("master copy = %q, %v", got, err)
	}
	if got, err := os.ReadFile(filepath.Join(dst, ".hidden")); err != nil || string(got) != "x" {
		t.Fatalf("hidden copy = %q, %v", got, err)
	}
	if _, err := os.Stat(filepath.Join(dst, "op_metric")); !os.IsNotExist(err) {
		t.Fatalf("op_metric copied to %s", dst)
	}
	if _, err := os.Stat(filepath.Join(dst, "analysis_result")); !os.IsNotExist(err) {
		t.Fatalf("analysis_result copied to %s", dst)
	}
	if symlinkCreated {
		if _, err := os.Stat(filepath.Join(dst, "link.db")); !os.IsNotExist(err) {
			t.Fatalf("symlink copied to %s", dst)
		}
	}
}

func readDumpMeta(t *testing.T, archive string) CycleResult {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(archive, "daemon_meta.json"))
	if err != nil {
		t.Fatalf("read daemon_meta.json: %v", err)
	}
	var meta CycleResult
	if err := json.Unmarshal(raw, &meta); err != nil {
		t.Fatalf("parse daemon_meta.json: %v", err)
	}
	return meta
}

func setFreeDisk(t *testing.T, free uint64, known bool) {
	t.Helper()
	old := freeDiskBytesFn
	freeDiskBytesFn = func(string) (uint64, bool) { return free, known }
	t.Cleanup(func() { freeDiskBytesFn = old })
}

func TestStoreProfilerDataUpdateUsesCopies(t *testing.T) {
	s := newStore()
	cr := &CycleResult{ID: 1, ProfilerDataStatus: "pending"}
	s.add(cr)
	snap := s.latest()
	if snap == cr {
		t.Fatal("latest must return a copy, not the stored pointer")
	}
	s.setProfilerData(1, profilerDataResult{
		Dump: true, Status: "ok", Bytes: 3, SourceBytes: 4,
	})
	if cr.ProfilerDataStatus != "ok" || cr.ProfilerDataBytes != 3 || cr.ProfilerDataSourceBytes != 4 {
		t.Fatalf("stored cycle not updated: status=%q bytes=%d source=%d",
			cr.ProfilerDataStatus, cr.ProfilerDataBytes, cr.ProfilerDataSourceBytes)
	}
	if snap.ProfilerDataStatus != "pending" {
		t.Fatalf("old snapshot mutated: status=%q", snap.ProfilerDataStatus)
	}
	if got := s.latest(); got.ProfilerDataStatus != "ok" || got.ProfilerDataBytes != 3 {
		t.Fatalf("new snapshot = %q/%d, want ok/3", got.ProfilerDataStatus, got.ProfilerDataBytes)
	}
}

func TestCopyProfilerDataRejectsDestinationInsideSource(t *testing.T) {
	src := t.TempDir()
	writeTestFile(t, filepath.Join(src, "master_0", "a.db"), "abc")
	dst := filepath.Join(src, "daemon_results", "20261126-100000", "profiler_data")
	if _, _, err := copyProfilerData(src, dst, noopLogf); err == nil {
		t.Fatal("copy inside the profiler root must fail")
	}
}

func TestDumpProfilerDataMetaIsPendingDuringCopy(t *testing.T) {
	src := t.TempDir()
	writeTestFile(t, filepath.Join(src, "master_0", "a.db"), "abc")
	archive := t.TempDir()
	setFreeDisk(t, 100<<30, true)

	d := New(Config{DumpProfilerData: true, ProfilerDir: src}, nil)
	d.logf = noopLogf
	cr := &CycleResult{ID: 1, StartedAt: time.Now(), DumpDir: archive,
		DumpProfilerData: true, ProfilerDataStatus: "pending"}
	d.finishCycle(cr)

	oldCopy := copyProfilerDataFn
	sawPending := false
	copyProfilerDataFn = func(s, dst string, logf func(string, ...any)) (int64, int64, error) {
		meta := readDumpMeta(t, archive)
		sawPending = meta.ProfilerDataStatus == "pending"
		return oldCopy(s, dst, logf)
	}
	t.Cleanup(func() { copyProfilerDataFn = oldCopy })

	d.cleanupDump(cr)
	if !sawPending {
		t.Fatal("daemon_meta.json was not pending while the copy ran")
	}
}

func TestDumpProfilerDataMissingRootSkipped(t *testing.T) {
	archive := t.TempDir()
	d := New(Config{DumpProfilerData: true, ProfilerDir: filepath.Join(t.TempDir(), "missing")}, nil)
	d.logf = noopLogf
	cr := &CycleResult{ID: 1, StartedAt: time.Now(), DumpDir: archive,
		DumpProfilerData: true, ProfilerDataStatus: "pending"}
	d.finishCycle(cr)
	d.cleanupDump(cr)
	if cr.ProfilerDataStatus != "skipped" || cr.ProfilerDataError != "no_raw_dump" {
		t.Fatalf("status/error = %q/%q, want skipped/no_raw_dump",
			cr.ProfilerDataStatus, cr.ProfilerDataError)
	}
}

func TestDumpProfilerDataCopiesAndMeta(t *testing.T) {
	src := t.TempDir()
	writeTestFile(t, filepath.Join(src, "master_0", "a.db"), "abc")
	writeTestFile(t, filepath.Join(src, "op_metric", "ignore"), "ignored")
	archive := t.TempDir()
	setFreeDisk(t, 100<<30, true)

	d := New(Config{DumpProfilerData: true, ProfilerDir: src}, nil)
	d.logf = noopLogf
	cr := &CycleResult{ID: 1, StartedAt: time.Now(), DumpDir: archive,
		DumpProfilerData: true, ProfilerDataStatus: "pending"}
	d.finishCycle(cr)
	d.cleanupDump(cr)

	if cr.ProfilerDataStatus != "ok" || cr.ProfilerDataBytes != 3 || cr.ProfilerDataError != "" {
		t.Fatalf("status/bytes/error = %q/%d/%q, want ok/3/empty",
			cr.ProfilerDataStatus, cr.ProfilerDataBytes, cr.ProfilerDataError)
	}
	if _, err := os.Stat(src); !os.IsNotExist(err) {
		t.Fatalf("raw profiler dir still exists after successful copy: %v", err)
	}
	if got, err := os.ReadFile(filepath.Join(archive, "profiler_data", "master_0", "a.db")); err != nil || string(got) != "abc" {
		t.Fatalf("copied data = %q, %v", got, err)
	}
	meta := readDumpMeta(t, archive)
	if !meta.DumpProfilerData || meta.ProfilerDataStatus != "ok" || meta.ProfilerDataBytes != 3 || meta.ProfilerDataError != "" {
		t.Fatalf("meta = %+v, want dump/ok/3/empty", meta)
	}
}

func TestDumpProfilerDataEmptyRootSkipped(t *testing.T) {
	src := t.TempDir()
	archive := t.TempDir()
	d := New(Config{DumpProfilerData: true, ProfilerDir: src}, nil)
	d.logf = noopLogf
	cr := &CycleResult{ID: 1, StartedAt: time.Now(), DumpDir: archive,
		DumpProfilerData: true, ProfilerDataStatus: "pending"}
	d.finishCycle(cr)
	d.cleanupDump(cr)

	if cr.ProfilerDataStatus != "skipped" || cr.ProfilerDataError != "no_raw_dump" {
		t.Fatalf("status/error = %q/%q, want skipped/no_raw_dump",
			cr.ProfilerDataStatus, cr.ProfilerDataError)
	}
	if _, err := os.Stat(filepath.Join(archive, "profiler_data")); !os.IsNotExist(err) {
		t.Fatal("empty root created profiler_data")
	}
}

func TestDumpProfilerDataOnlyExcludedDirsSkipped(t *testing.T) {
	src := t.TempDir()
	writeTestFile(t, filepath.Join(src, "op_metric", "ignore"), "ignored")
	archive := t.TempDir()
	setFreeDisk(t, 100<<30, true)

	d := New(Config{DumpProfilerData: true, ProfilerDir: src}, nil)
	d.logf = noopLogf
	cr := &CycleResult{ID: 1, StartedAt: time.Now(), DumpDir: archive,
		DumpProfilerData: true, ProfilerDataStatus: "pending"}
	d.finishCycle(cr)
	d.cleanupDump(cr)

	if cr.ProfilerDataStatus != "skipped" || cr.ProfilerDataError != "no_raw_dump" {
		t.Fatalf("status/error = %q/%q, want skipped/no_raw_dump",
			cr.ProfilerDataStatus, cr.ProfilerDataError)
	}
	if _, err := os.Stat(filepath.Join(archive, "profiler_data")); !os.IsNotExist(err) {
		t.Fatal("only-excluded source created profiler_data")
	}
}

func TestDumpProfilerDataCopiesFailedCycle(t *testing.T) {
	src := t.TempDir()
	writeTestFile(t, filepath.Join(src, "master_0", "a.db"), "abc")
	archive := t.TempDir()
	setFreeDisk(t, 100<<30, true)

	d := New(Config{DumpProfilerData: true, ProfilerDir: src}, nil)
	d.logf = noopLogf
	cr := &CycleResult{ID: 1, StartedAt: time.Now(), DumpDir: archive,
		DumpProfilerData: true, ProfilerDataStatus: "pending", Error: "parse_failed"}
	d.finishCycle(cr)
	d.cleanupDump(cr)

	if cr.ProfilerDataStatus != "ok" {
		t.Fatalf("failed cycle profiler_data status = %q, want ok", cr.ProfilerDataStatus)
	}
	if _, err := os.Stat(filepath.Join(archive, "profiler_data", "master_0", "a.db")); err != nil {
		t.Fatalf("failed cycle profiler_data missing: %v", err)
	}
}

func TestDumpProfilerDataLowDiskSkipsAndDeletes(t *testing.T) {
	src := t.TempDir()
	writeTestFile(t, filepath.Join(src, "master_0", "a.db"), "abc")
	archive := t.TempDir()
	setFreeDisk(t, 49<<30, true)

	d := New(Config{DumpProfilerData: true, ProfilerDir: src}, nil)
	d.logf = noopLogf
	cr := &CycleResult{ID: 1, StartedAt: time.Now(), DumpDir: archive,
		DumpProfilerData: true, ProfilerDataStatus: "pending"}
	d.finishCycle(cr)
	d.cleanupDump(cr)

	if cr.ProfilerDataStatus != "skipped" || cr.ProfilerDataBytes != 0 {
		t.Fatalf("status/bytes = %q/%d, want skipped/0", cr.ProfilerDataStatus, cr.ProfilerDataBytes)
	}
	if !strings.Contains(cr.ProfilerDataError, "skipped_low_disk") {
		t.Fatalf("error = %q, want skipped_low_disk", cr.ProfilerDataError)
	}
	if _, err := os.Stat(src); !os.IsNotExist(err) {
		t.Fatalf("low-disk skip must delete the raw dump: %v", err)
	}
	if _, err := os.Stat(filepath.Join(archive, "profiler_data")); !os.IsNotExist(err) {
		t.Fatalf("low-disk skip created profiler_data")
	}
	meta := readDumpMeta(t, archive)
	if meta.ProfilerDataStatus != "skipped" {
		t.Fatalf("meta status = %q, want skipped", meta.ProfilerDataStatus)
	}
}

func TestDumpProfilerDataDefaultDisabled(t *testing.T) {
	src := t.TempDir()
	writeTestFile(t, filepath.Join(src, "master_0", "a.db"), "abc")
	archive := t.TempDir()

	d := New(Config{ProfilerDir: src}, nil)
	d.logf = noopLogf
	cr := &CycleResult{ID: 1, StartedAt: time.Now(), DumpDir: archive, ProfilerDataStatus: "disabled"}
	d.finishCycle(cr)
	d.cleanupDump(cr)

	if cr.ProfilerDataStatus != "disabled" || cr.ProfilerDataBytes != 0 {
		t.Fatalf("status/bytes = %q/%d, want disabled/0", cr.ProfilerDataStatus, cr.ProfilerDataBytes)
	}
	if _, err := os.Stat(src); !os.IsNotExist(err) {
		t.Fatalf("disabled dump must still delete the raw dump: %v", err)
	}
	if _, err := os.Stat(filepath.Join(archive, "profiler_data")); !os.IsNotExist(err) {
		t.Fatalf("disabled dump created profiler_data")
	}
	meta := readDumpMeta(t, archive)
	if meta.DumpProfilerData || meta.ProfilerDataStatus != "disabled" {
		t.Fatalf("meta = %+v, want dump=false/disabled", meta)
	}
}

func TestPrepareProfilerRootRenamesLeftover(t *testing.T) {
	root := filepath.Join(t.TempDir(), "profiler")
	writeTestFile(t, filepath.Join(root, "master_0", "a.db"), "abc")
	d := New(Config{DumpProfilerData: true, ProfilerDir: root}, nil)
	d.logf = noopLogf

	if err := d.prepareProfilerRoot(); err != nil {
		t.Fatalf("prepareProfilerRoot: %v", err)
	}
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Fatalf("raw root still exists: %v", err)
	}
	stale, err := filepath.Glob(root + ".stale-*")
	if err != nil || len(stale) != 1 {
		t.Fatalf("stale dirs = %v, %v; want 1", stale, err)
	}
	if got, err := os.ReadFile(filepath.Join(stale[0], "master_0", "a.db")); err != nil || string(got) != "abc" {
		t.Fatalf("stale data = %q, %v", got, err)
	}
}

func TestStaleRenameFailureFailsCycle(t *testing.T) {
	oldWD, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chdir(oldWD) }()

	root := filepath.Join(t.TempDir(), "profiler")
	writeTestFile(t, filepath.Join(root, "master_0", "a.db"), "abc")
	oldRename := renameFn
	renameFn = func(string, string) error { return errors.New("rename boom") }
	t.Cleanup(func() { renameFn = oldRename })

	d := New(Config{DumpProfilerData: true, ProfilerDir: root}, nil)
	d.logf = noopLogf
	d.runCycle(1, 3, "")

	cr := d.st.latest()
	if cr == nil {
		t.Fatal("no cycle stored")
	}
	if !strings.Contains(cr.Error, "stale_dump_present") {
		t.Fatalf("cycle error = %q, want stale_dump_present", cr.Error)
	}
	if cr.ProfilerDataStatus != "failed" || !cr.keepRawDump {
		t.Fatalf("status/keep = %q/%v, want failed/true", cr.ProfilerDataStatus, cr.keepRawDump)
	}
	if _, err := os.Stat(root); err != nil {
		t.Fatalf("raw root must remain for manual cleanup: %v", err)
	}
}

func TestCleanProfilerDataCopiesKeepsFive(t *testing.T) {
	base := t.TempDir()
	var names []string
	for i := 0; i < 7; i++ {
		name := fmt.Sprintf("20261126-10000%d", i)
		names = append(names, name)
		writeTestFile(t, filepath.Join(base, name, "profiler_data", "master_0", "a.db"), "x")
	}
	cleanProfilerDataCopies(base, 5, "", noopLogf)
	for i, name := range names {
		_, err := os.Stat(filepath.Join(base, name, "profiler_data"))
		if i < 2 {
			if !os.IsNotExist(err) {
				t.Fatalf("old copy %s still exists", name)
			}
		} else if err != nil {
			t.Fatalf("new copy %s missing: %v", name, err)
		}
	}
	if _, err := os.Stat(filepath.Join(base, names[0])); err != nil {
		t.Fatalf("parent cycle dir %s was removed: %v", names[0], err)
	}
}

func TestCleanProfilerDataCopiesProtectsCurrentCopy(t *testing.T) {
	base := t.TempDir()
	old := filepath.Join(base, "20260101-000000", "profiler_data")
	writeTestFile(t, filepath.Join(old, "master_0", "a.db"), "x")
	for i := 0; i < 6; i++ {
		writeTestFile(t, filepath.Join(base, fmt.Sprintf("20260102-00000%d", i), "profiler_data", "master_0", "a.db"), "x")
	}
	cleanProfilerDataCopies(base, 5, old, noopLogf)

	if _, err := os.Stat(old); err != nil {
		t.Fatalf("protected current copy was removed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(base, "20260102-000000", "profiler_data")); !os.IsNotExist(err) {
		t.Fatal("oldest new copy was not pruned")
	}
	if _, err := os.Stat(filepath.Join(base, "20260102-000001", "profiler_data")); !os.IsNotExist(err) {
		t.Fatal("retention did not make room for the protected copy")
	}
	matches, _ := filepath.Glob(filepath.Join(base, "*", "profiler_data"))
	if len(matches) != 5 {
		t.Fatalf("retained copies = %d, want 5", len(matches))
	}
}

func TestWarnStaleProfilerDirsKeepsThem(t *testing.T) {
	root := filepath.Join(t.TempDir(), "profiler")
	stale := root + ".stale-20261126-100000"
	writeTestFile(t, filepath.Join(stale, "master_0", "a.db"), "abc")
	var logs []string
	warnStaleProfilerDirs(root, func(format string, args ...any) {
		logs = append(logs, fmt.Sprintf(format, args...))
	})
	if _, err := os.Stat(stale); err != nil {
		t.Fatalf("stale dir was removed: %v", err)
	}
	joined := strings.Join(logs, "\n")
	if !strings.Contains(joined, "stale profiler data present") || !strings.Contains(joined, "3 B") {
		t.Fatalf("warn logs = %q, want path and size", joined)
	}
}

func TestDumpProfilerDataCopyFailureKeepsDump(t *testing.T) {
	src := t.TempDir()
	writeTestFile(t, filepath.Join(src, "master_0", "a.db"), "abc")
	archive := t.TempDir()
	setFreeDisk(t, 100<<30, true)
	oldCopy := copyProfilerDataFn
	copyProfilerDataFn = func(string, string, func(string, ...any)) (int64, int64, error) {
		return 0, 3, errors.New("copy boom")
	}
	t.Cleanup(func() { copyProfilerDataFn = oldCopy })

	d := New(Config{DumpProfilerData: true, ProfilerDir: src}, nil)
	d.logf = noopLogf
	cr := &CycleResult{ID: 1, StartedAt: time.Now(), DumpDir: archive,
		DumpProfilerData: true, ProfilerDataStatus: "pending"}
	d.finishCycle(cr)
	d.cleanupDump(cr)

	if cr.ProfilerDataStatus != "failed" || cr.ProfilerDataError == "" || cr.ProfilerDataBytes != 0 {
		t.Fatalf("status/error/bytes = %q/%q/%d, want failed/non-empty/0",
			cr.ProfilerDataStatus, cr.ProfilerDataError, cr.ProfilerDataBytes)
	}
	if _, err := os.Stat(src); err != nil {
		t.Fatalf("copy failure must keep the raw dump: %v", err)
	}
	if _, err := os.Stat(filepath.Join(archive, "profiler_data")); !os.IsNotExist(err) {
		t.Fatalf("partial profiler_data must be removed")
	}
	meta := readDumpMeta(t, archive)
	if meta.ProfilerDataStatus != "failed" {
		t.Fatalf("meta status = %q, want failed", meta.ProfilerDataStatus)
	}

	if err := d.prepareProfilerRoot(); err != nil {
		t.Fatalf("prepare stale root: %v", err)
	}
	if _, err := os.Stat(src); !os.IsNotExist(err) {
		t.Fatalf("failed copy's raw root was not moved aside: %v", err)
	}
	stale, err := filepath.Glob(src + ".stale-*")
	if err != nil || len(stale) != 1 {
		t.Fatalf("stale dirs = %v, %v; want 1", stale, err)
	}
}
