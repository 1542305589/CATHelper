package daemon

import (
	"os"
	"path/filepath"
	"testing"
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
