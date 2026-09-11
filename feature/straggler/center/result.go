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
	base := filepath.Join(c.cfg.DataDir, name)
	entries, err := os.ReadDir(base)
	if err != nil {
		return ""
	}
	var dirs []string
	for _, e := range entries {
		if e.IsDir() {
			dirs = append(dirs, e.Name())
		}
	}
	if len(dirs) == 0 {
		return ""
	}
	sort.Strings(dirs)
	return filepath.Join(base, dirs[len(dirs)-1])
}