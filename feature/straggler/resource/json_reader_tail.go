package resource

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
)

// DefaultKPITailBytes is the per-file tail window used by the resident daemon.
// 1 MiB is roughly 200 KPI samples at the current 1-2 lines/s rate, which
// comfortably covers the 10s aggregation window without reading the full
// multi-day history.
const DefaultKPITailBytes = 1 << 20

// ReadKPIFilesTail is the daemon-mode counterpart of ReadKPIFiles: it reads only
// the tail of the newest 1-2 straggler_kpi_*.jsonl files (per node when
// node_config.json is present) instead of the whole history. Detection only uses
// the last aggregated point, so this preserves the detection result while
// keeping the read O(tailBytes) instead of O(history).
//
// ReadKPIFiles (one-shot / full-history callers) is intentionally unchanged.
func ReadKPIFilesTail(dir string, tailBytes int64) (*TimeSeriesData, error) {
	if tailBytes <= 0 {
		tailBytes = DefaultKPITailBytes
	}
	idx := newCardIndexer()
	var rows []CSVRow

	cfg, hasCfg, err := readJSONLNodeConfig(dir)
	if err != nil {
		return nil, err
	}
	if hasCfg {
		// Sort folders so global card IDs are deterministic across runs.
		folders := make([]string, 0, len(cfg))
		for folder := range cfg {
			folders = append(folders, folder)
		}
		sort.Strings(folders)
		for _, folder := range folders {
			nc := cfg[folder]
			if nc.Node == "" {
				return nil, fmt.Errorf("node_config.json: folder %q missing node name", folder)
			}
			folderPath := filepath.Join(dir, folder)
			info, serr := os.Stat(folderPath)
			if serr != nil {
				fmt.Fprintf(os.Stderr, "[SLOWNODE ALGO] [WARN] node_config.json folder %q in %s: %v, skipping\n", folder, dir, serr)
				continue
			}
			if !info.IsDir() {
				fmt.Fprintf(os.Stderr, "[SLOWNODE ALGO] [WARN] node_config.json folder %q in %s is not a directory, skipping\n", folder, dir)
				continue
			}
			allowed := make(map[int]bool, len(nc.Cards))
			for _, c := range nc.Cards {
				allowed[c] = true
			}
			fileRows, rerr := readLatestKPIFilesTail(folderPath, tailBytes, idx, nc.Node, allowed)
			if rerr != nil {
				return nil, rerr
			}
			rows = append(rows, fileRows...)
		}
	} else {
		fileRows, rerr := readLatestKPIFilesTail(dir, tailBytes, idx, "", nil)
		if rerr != nil {
			return nil, rerr
		}
		rows = append(rows, fileRows...)
	}

	if len(rows) == 0 {
		return nil, fmt.Errorf("no KPI samples in %s", dir)
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Timestamp < rows[j].Timestamp })

	return &TimeSeriesData{
		Rows:    rows,
		RawRows: rows,
		CardIDs: idx.sortedGlobalIDs(),
		NodeOf:  idx.nodeMap(),
		LocalID: idx.localMap(),
	}, nil
}

// readLatestKPIFilesTail reads at most the two newest files in dir. It stops at
// the newest file when that file produced card samples; otherwise it falls back
// to the previous file (e.g. the newest file has only cpu_avg rows).
func readLatestKPIFilesTail(dir string, tailBytes int64, idx *cardIndexer, node string, allowed map[int]bool) ([]CSVRow, error) {
	files := listJSONLFiles(dir)
	var rows []CSVRow
	attempts := 0
	for i := len(files) - 1; i >= 0 && attempts < 2; i-- {
		attempts++
		fileRows, hasVals, err := readKPIFileTail(files[i], tailBytes, idx, node, allowed)
		if err != nil {
			return nil, err
		}
		rows = append(rows, fileRows...)
		if hasVals {
			break
		}
	}
	return rows, nil
}

// readKPIFileTail reads the last tailBytes of one JSONL file and returns the
// complete lines in it. If the read started mid-line, the leading partial line
// is dropped; a trailing partial line (no final newline) is also dropped.
func readKPIFileTail(path string, tailBytes int64, idx *cardIndexer, node string, allowed map[int]bool) ([]CSVRow, bool, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("cannot open %s: %w", path, err)
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return nil, false, fmt.Errorf("stat %s: %w", path, err)
	}
	size := info.Size()
	if size <= 0 {
		return nil, false, nil
	}
	start := int64(0)
	if size > tailBytes {
		start = size - tailBytes
	}
	if _, err := f.Seek(start, io.SeekStart); err != nil {
		return nil, false, fmt.Errorf("seek %s: %w", path, err)
	}
	buf, err := io.ReadAll(io.LimitReader(f, size-start))
	if err != nil {
		return nil, false, fmt.Errorf("read %s: %w", path, err)
	}

	if start > 0 {
		i := bytes.IndexByte(buf, '\n')
		if i < 0 {
			return nil, false, nil // the tail is one incomplete line
		}
		buf = buf[i+1:]
	}
	if len(buf) > 0 && buf[len(buf)-1] != '\n' {
		i := bytes.LastIndexByte(buf, '\n')
		if i < 0 {
			return nil, false, nil // no complete line
		}
		buf = buf[:i+1]
	}

	var rows []CSVRow
	hasVals := false
	for _, raw := range bytes.Split(buf, []byte{'\n'}) {
		line := bytes.TrimSpace(raw)
		if len(line) == 0 {
			continue
		}
		var s KPISample
		if err := json.Unmarshal(line, &s); err != nil {
			fmt.Fprintf(os.Stderr, "[SLOWNODE ALGO] [WARN] skipping bad kpi line in %s: %v\n", path, err)
			continue
		}
		// cpu_avg-only rows carry no detection data; skipping them keeps the
		// last aggregated point from becoming an empty row.
		if len(s.Vals) == 0 {
			continue
		}
		rows = append(rows, sampleToRow(s, idx, node, allowed))
		hasVals = true
	}
	return rows, hasVals, nil
}
