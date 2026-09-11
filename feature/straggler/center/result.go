package center

import (
	"os"
	"path/filepath"
	"sort"
)

// latestResultDir returns the newest per-cycle result dir under a business, or
// "" when none exists. Result dirs are named by timestamp (20060102-150405), so
// lexicographic order == chronological order.
func (c *Center) latestResultDir(name string) string {
	dirs := c.resultDirs(name)
	if len(dirs) == 0 {
		return ""
	}
	return filepath.Join(c.cfg.DataDir, name, dirs[0])
}

// resultDirs returns the per-cycle result dir names (timestamps) under a
// business, newest first.
func (c *Center) resultDirs(name string) []string {
	base := filepath.Join(c.cfg.DataDir, name)
	entries, err := os.ReadDir(base)
	if err != nil {
		return nil
	}
	var dirs []string
	for _, e := range entries {
		if e.IsDir() {
			dirs = append(dirs, e.Name())
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(dirs)))
	return dirs
}

// resultDir returns the result dir for a specific timestamp, or the latest when
// ts is empty; "" when absent.
func (c *Center) resultDir(name, ts string) string {
	if ts == "" {
		return c.latestResultDir(name)
	}
	dir := filepath.Join(c.cfg.DataDir, name, ts)
	if info, err := os.Stat(dir); err == nil && info.IsDir() {
		return dir
	}
	return ""
}