// Package utils provides result-writing and utility functions for the
// straggler detection system.
package utils

import (
	"os"
	"sort"
	"strconv"
	"strings"
)

// ---------------------------------------------------------------------------
// Utility functions
// ---------------------------------------------------------------------------

// CheckFileOrDirectoryReadMode returns true if the path exists and is readable.
func CheckFileOrDirectoryReadMode(path string) bool {
	info, err := os.Stat(path)
	if err != nil {
		return false
	}
	_ = info
	return true
}

// CheckFileOrDirectoryIsSoftLink returns true if path is a symbolic link.
func CheckFileOrDirectoryIsSoftLink(path string) bool {
	info, err := os.Lstat(path)
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeSymlink != 0
}

// TransferFloatArrayToInt converts []interface{} containing float64 values
// (typical of JSON unmarshalling) to []int.
func TransferFloatArrayToInt(ids []interface{}) []int {
	result := make([]int, 0, len(ids))
	for _, v := range ids {
		switch n := v.(type) {
		case float64:
			result = append(result, int(n))
		case int:
			result = append(result, n)
		}
	}
	return result
}

// ReadFile reads an entire file and returns its content.
func ReadFile(filePath string) ([]byte, error) {
	return os.ReadFile(filePath)
}

// ---------------------------------------------------------------------------
// Communication domain resolution (shared with the node output writer)
// ---------------------------------------------------------------------------

// findDomainForRanks finds which parallel domain a sorted rank set belongs to.
func findDomainForRanks(ranks []int, parallels map[string][][]int) string {
	for domain, groups := range parallels {
		for _, group := range groups {
			if intSlicesEqual(sortInts(group), ranks) {
				return domain
			}
		}
	}
	return ""
}

// stringToRanks parses a comma-separated rank key (e.g. "0,2,4") into []int.
func stringToRanks(s string) []int {
	parts := strings.Split(s, ",")
	ranks := make([]int, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		n, err := strconv.Atoi(p)
		if err != nil {
			return nil
		}
		ranks = append(ranks, n)
	}
	return ranks
}

func sortInts(a []int) []int {
	sorted := make([]int, len(a))
	copy(sorted, a)
	sort.Ints(sorted)
	return sorted
}

func intSlicesEqual(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
