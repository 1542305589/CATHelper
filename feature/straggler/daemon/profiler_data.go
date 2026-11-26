package daemon

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// copyProfilerData copies the raw profiler root into dst, excluding the two
// top-level directories that already live in the cycle archive. It returns the
// bytes copied and the source bytes considered (both excluding skipped data).
func copyProfilerData(src, dst string, logf func(string, ...any)) (copied, source int64, err error) {
	source, err = profilerDataSourceBytes(src)
	if err != nil {
		return 0, 0, err
	}
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return 0, source, err
	}
	state := &copyState{total: source, last: time.Now(), logf: logf}
	err = filepath.Walk(src, func(path string, fi os.FileInfo, werr error) error {
		if werr != nil {
			return werr
		}
		rel, rerr := filepath.Rel(src, path)
		if rerr != nil {
			return rerr
		}
		if rel == "." {
			return nil
		}
		if skipProfilerTopLevel(rel) {
			if fi.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if fi.Mode()&os.ModeSymlink != 0 {
			logf("skip symlink in profiler data: %s", path)
			return nil
		}
		out := filepath.Join(dst, rel)
		if fi.IsDir() {
			return os.MkdirAll(out, 0o755)
		}
		return copyFileStream(path, out, fi.Mode(), state)
	})
	return state.copied, source, err
}

// profilerDataSourceBytes sums the regular files that copyProfilerData will
// copy, using the same top-level exclusions and symlink rule.
func profilerDataSourceBytes(src string) (int64, error) {
	var total int64
	err := filepath.Walk(src, func(path string, fi os.FileInfo, werr error) error {
		if werr != nil {
			return werr
		}
		rel, rerr := filepath.Rel(src, path)
		if rerr != nil {
			return rerr
		}
		if rel == "." {
			return nil
		}
		if skipProfilerTopLevel(rel) {
			if fi.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if fi.Mode()&os.ModeSymlink != 0 || fi.IsDir() {
			return nil
		}
		total += fi.Size()
		return nil
	})
	if err != nil {
		return 0, err
	}
	return total, nil
}

func skipProfilerTopLevel(rel string) bool {
	top := rel
	if i := strings.IndexRune(rel, os.PathSeparator); i >= 0 {
		top = rel[:i]
	}
	return top == "op_metric" || top == "analysis_result"
}

type copyState struct {
	copied int64
	total  int64
	last   time.Time
	logf   func(string, ...any)
}

type progressWriter struct {
	dst   io.Writer
	state *copyState
}

func (w *progressWriter) Write(p []byte) (int, error) {
	n, err := w.dst.Write(p)
	w.state.copied += int64(n)
	if time.Since(w.state.last) >= 30*time.Second {
		w.state.logf("profiler_data copy progress: %d/%d bytes", w.state.copied, w.state.total)
		w.state.last = time.Now()
	}
	return n, err
}

func copyFileStream(src, dst string, mode os.FileMode, state *copyState) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode.Perm())
	if err != nil {
		return err
	}
	if _, err := io.Copy(&progressWriter{dst: out, state: state}, in); err != nil {
		_ = out.Close()
		return fmt.Errorf("copy %s: %w", src, err)
	}
	if err := out.Close(); err != nil {
		return fmt.Errorf("close %s: %w", dst, err)
	}
	return nil
}
