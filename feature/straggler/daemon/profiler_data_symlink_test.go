//go:build !windows

package daemon

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestCopyProfilerDataSkipsSymlink(t *testing.T) {
	src := t.TempDir()
	writeTestFile(t, filepath.Join(src, "master_0", "a.db"), "abc")
	if err := os.Symlink(filepath.Join(src, "master_0", "a.db"), filepath.Join(src, "link.db")); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(t.TempDir(), "profiler_data")
	copied, source, err := copyProfilerData(src, dst, noopLogf)
	if err != nil {
		t.Fatalf("copyProfilerData: %v", err)
	}
	if copied != 3 || source != 3 {
		t.Fatalf("copied/source = %d/%d, want 3/3", copied, source)
	}
	if _, err := os.Stat(filepath.Join(dst, "link.db")); !os.IsNotExist(err) {
		t.Fatalf("symlink was copied: %v", err)
	}
}

func TestCopyProfilerDataSkipsFIFO(t *testing.T) {
	src := t.TempDir()
	writeTestFile(t, filepath.Join(src, "master_0", "a.db"), "abc")
	fifo := filepath.Join(src, "pipe.db")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(t.TempDir(), "profiler_data")
	copied, source, err := copyProfilerData(src, dst, noopLogf)
	if err != nil {
		t.Fatalf("copyProfilerData: %v", err)
	}
	if copied != 3 || source != 3 {
		t.Fatalf("copied/source = %d/%d, want 3/3", copied, source)
	}
	if _, err := os.Stat(filepath.Join(dst, "pipe.db")); !os.IsNotExist(err) {
		t.Fatalf("FIFO was copied: %v", err)
	}
}
