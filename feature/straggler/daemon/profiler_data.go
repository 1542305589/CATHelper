package daemon

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// copyProfilerDataFn and renameFn are test seams.
var (
	copyProfilerDataFn = copyProfilerData
	renameFn           = os.Rename
)

// prepareProfilerRoot moves a leftover raw dump aside before dyno runs. A
// rename failure must fail the cycle rather than letting dyno write into a
// dirty root.
func (d *Daemon) prepareProfilerRoot() error {
	if !d.cfg.DumpProfilerData {
		return nil
	}
	root := d.cfg.ProfilerDir
	entries, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if len(entries) == 0 {
		return nil
	}
	base := root + ".stale-" + time.Now().Format("20060102-150405")
	target := base
	for i := 1; ; i++ {
		if _, err := os.Stat(target); os.IsNotExist(err) {
			break
		} else if err != nil {
			return err
		}
		target = fmt.Sprintf("%s-%d", base, i)
	}
	if err := renameFn(root, target); err != nil {
		return err
	}
	d.logf("moved stale profiler data: %s -> %s", root, target)
	warnStaleProfilerDirs(root, d.logf)
	return nil
}

type profilerDataResult struct {
	Dump        bool
	Status      string
	Bytes       int64
	SourceBytes int64
	Error       string
	KeepDump    bool
}

// applyProfilerDataPolicy copies the raw profiler root before cleanup (when
// enabled), or records why it was skipped/failed. It does not mutate the
// published CycleResult; cleanupDump applies the result through the store.
func (d *Daemon) applyProfilerDataPolicy(cr *CycleResult) profilerDataResult {
	res := profilerDataResult{Dump: d.cfg.DumpProfilerData}
	if !d.cfg.DumpProfilerData {
		res.Status = "disabled"
		return res
	}
	if cr.keepRawDump {
		res.Status = "failed"
		res.Error = cr.ProfilerDataError
		if res.Error == "" {
			res.Error = "stale_dump_present"
		}
		res.KeepDump = true
		return res
	}
	root := d.cfg.ProfilerDir
	if !fileExistsDir(root) || !dirHasEntries(root) {
		res.Status = "skipped"
		res.Error = "no_raw_dump"
		d.logf("cycle %d profiler_data skipped: no raw dump under %s", cr.ID, root)
		return res
	}
	if cr.DumpDir == "" {
		res.Status = "failed"
		res.Error = "archive directory is empty"
		res.KeepDump = true
		return res
	}
	if err := os.MkdirAll(cr.DumpDir, 0o755); err != nil {
		res.Status = "failed"
		res.Error = fmt.Sprintf("mkdir archive: %v", err)
		res.KeepDump = true
		return res
	}
	dst := filepath.Join(cr.DumpDir, "profiler_data")
	_ = os.RemoveAll(dst)
	copied, source, err := copyProfilerDataFn(root, dst, d.logf)
	res.SourceBytes = source
	if err != nil {
		_ = os.RemoveAll(dst)
		res.Status = "failed"
		res.Error = err.Error()
		res.KeepDump = true
		d.logf("cycle %d profiler_data copy failed: %v; keeping raw dump", cr.ID, err)
		return res
	}
	if copied == 0 {
		_ = os.RemoveAll(dst)
		res.Status = "skipped"
		res.Error = "no_raw_dump"
		d.logf("cycle %d profiler_data skipped: no regular files to copy", cr.ID)
		return res
	}
	res.Status = "ok"
	res.Bytes = copied
	d.logf("cycle %d profiler_data copied: %d/%d bytes -> %s", cr.ID, copied, source, dst)
	return res
}

// copyProfilerData copies the raw profiler root into dst, excluding the two
// top-level directories that already live in the cycle archive. It returns the
// bytes copied and the source bytes considered (both excluding skipped data).
func copyProfilerData(src, dst string, logf func(string, ...any)) (copied, source int64, err error) {
	absSrc, err := filepath.Abs(src)
	if err != nil {
		return 0, 0, err
	}
	absDst, err := filepath.Abs(dst)
	if err != nil {
		return 0, 0, err
	}
	if rel, rerr := filepath.Rel(absSrc, absDst); rerr == nil &&
		rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return 0, 0, fmt.Errorf("refusing to copy into a directory inside the profiler root: %s", dst)
	}
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
		if !fi.Mode().IsRegular() {
			logf("skip non-regular profiler data: %s (%s)", path, fi.Mode())
			return nil
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
		if fi.Mode()&os.ModeSymlink != 0 || fi.IsDir() || !fi.Mode().IsRegular() {
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

// warnStaleProfilerDirs logs stale raw dumps but never deletes them.
func warnStaleProfilerDirs(root string, logf func(string, ...any)) {
	matches, err := filepath.Glob(root + ".stale-*")
	if err != nil {
		logf("stale profiler_data glob %s: %v", root, err)
		return
	}
	for _, path := range matches {
		logf("stale profiler data present: %s", path)
	}
}

func dirHasEntries(path string) bool {
	entries, err := os.ReadDir(path)
	return err == nil && len(entries) > 0
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
