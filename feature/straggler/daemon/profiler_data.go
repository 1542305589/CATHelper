package daemon

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	profilerDataKeep = 5
	minFreeDiskBytes = 50 * 1024 * 1024 * 1024
)

// freeDiskBytesFn and copyProfilerDataFn are test seams for the build-tagged
// disk check and the copy loop.
var (
	freeDiskBytesFn    = freeDiskBytes
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
	if free, known := freeDiskBytesFn(cr.DumpDir); known && free < minFreeDiskBytes {
		res.Status = "skipped"
		res.Error = fmt.Sprintf("skipped_low_disk: free=%d < %d", free, minFreeDiskBytes)
		d.logf("WARNING: cycle %d skip profiler_data copy: free disk %d < %d", cr.ID, free, minFreeDiskBytes)
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
	// Only prune the real daemon_results tree; tests use temp archive roots.
	if base := filepath.Dir(cr.DumpDir); filepath.Base(base) == "daemon_results" {
		cleanProfilerDataCopies(base, profilerDataKeep, dst, d.logf)
	}
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

// cleanProfilerDataCopies keeps the newest keep profiler_data dirs under
// base/<timestamp>/profiler_data, ordered by the timestamp directory name.
func (d *Daemon) cleanProfilerDataCopies() {
	cleanProfilerDataCopies("daemon_results", profilerDataKeep, "", d.logf)
}

func cleanProfilerDataCopies(base string, keep int, protect string, logf func(string, ...any)) {
	matches, err := filepath.Glob(filepath.Join(base, "*", "profiler_data"))
	if err != nil {
		logf("profiler_data retention glob: %v", err)
		return
	}
	type entry struct {
		path string
		name string
	}
	entries := make([]entry, 0, len(matches))
	for _, path := range matches {
		fi, err := os.Stat(path)
		if err != nil || !fi.IsDir() {
			continue
		}
		entries = append(entries, entry{path: path, name: filepath.Base(filepath.Dir(path))})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].name > entries[j].name })
	if keep < 0 {
		keep = 0
	}
	keepPaths := make(map[string]bool, keep+1)
	for i := 0; i < len(entries) && i < keep; i++ {
		keepPaths[entries[i].path] = true
	}
	if protect != "" {
		protect = filepath.Clean(protect)
		found := false
		for _, e := range entries {
			if e.path == protect {
				found = true
				break
			}
		}
		if found && !keepPaths[protect] {
			if keep > 0 && keep <= len(entries) {
				delete(keepPaths, entries[keep-1].path)
			}
			keepPaths[protect] = true
		}
	}
	for _, e := range entries {
		if keepPaths[e.path] {
			continue
		}
		if err := os.RemoveAll(e.path); err != nil {
			logf("profiler_data retention remove %s: %v", e.path, err)
			continue
		}
		logf("profiler_data retention removed oldest copy: %s", e.path)
	}
}

// warnStaleProfilerDirs logs stale raw dumps but never deletes them.
func warnStaleProfilerDirs(root string, logf func(string, ...any)) {
	matches, err := filepath.Glob(root + ".stale-*")
	if err != nil {
		logf("stale profiler_data glob %s: %v", root, err)
		return
	}
	for _, path := range matches {
		logf("stale profiler data present: %s (%s)", path, humanBytes(dirSize(path)))
	}
	if len(matches) > profilerDataKeep {
		logf("WARNING: %d stale profiler dirs (> %d), manual cleanup recommended", len(matches), profilerDataKeep)
	}
}

func dirHasEntries(path string) bool {
	entries, err := os.ReadDir(path)
	return err == nil && len(entries) > 0
}

func dirSize(path string) int64 {
	var total int64
	_ = filepath.Walk(path, func(_ string, fi os.FileInfo, err error) error {
		if err == nil && fi.Mode().IsRegular() {
			total += fi.Size()
		}
		return nil
	})
	return total
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
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
