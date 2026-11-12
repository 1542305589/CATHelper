package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	kpiWatchInterval = 200 * time.Millisecond
	kpiReadChunk     = 4 << 20 // at most 4 MiB of new lines per tick
	kpiBatchSize     = 500
	kpiMaxPending    = 10000
	kpiPostTimeout   = 10 * time.Second
	kpiErrLogEvery   = 30 * time.Second
)

// kpiShipSample is one NPU sample sent to the center. CPU-only JSONL lines are
// filtered out before this struct is built.
type kpiShipSample struct {
	Ts   int64                         `json:"ts"`
	Vals map[string]map[string]float64 `json:"vals,omitempty"`
}

// kpiShipReport is the daemon → center POST body.
type kpiShipReport struct {
	Node    string          `json:"node"`
	SentAt  int64           `json:"sent_at,omitempty"`
	Samples []kpiShipSample `json:"samples"`
}

// kpiTailer follows the newest straggler_kpi_*.jsonl file by inode+size+offset.
// It only consumes complete (newline-terminated) lines; a trailing partial line is
// left for the next tick.
type kpiTailer struct {
	dir     string
	path    string
	info    os.FileInfo
	offset  int64
	started bool
}

// readNew returns NPU samples from the newly appended complete lines. The first
// call after a daemon start anchors at EOF (no historical backfill); after a file
// rotation it starts at offset 0 so the new file is consumed from its beginning.
func (t *kpiTailer) readNew() ([]kpiShipSample, error) {
	path, err := latestKPIFileByModTime(t.dir)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	if !t.started {
		t.started = true
		t.path = path
		t.info = info
		t.offset = info.Size()
		return nil, nil
	}
	if t.path != path || t.info == nil || !os.SameFile(t.info, info) {
		// New day / rename / delete+recreate: consume the new file from the start.
		t.path = path
		t.info = info
		t.offset = 0
	}
	if info.Size() < t.offset {
		// Truncated in place.
		t.offset = 0
	}
	if info.Size() == t.offset {
		t.info = info
		return nil, nil
	}

	samples, newOffset, err := readKPILines(path, t.offset, info.Size())
	if err != nil {
		return nil, err
	}
	t.offset = newOffset
	t.info = info
	return samples, nil
}

// latestKPIFileByModTime returns the newest straggler_kpi_*.jsonl by mtime. mtime
// (not just the date suffix) avoids switching away from a file that is still
// being appended during the midnight rollover.
func latestKPIFileByModTime(dir string) (string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", fmt.Errorf("read kpi dir: %w", err)
	}
	type candidate struct {
		path string
		mod  time.Time
		name string
	}
	var candidates []candidate
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasPrefix(name, "straggler_kpi_") || !strings.HasSuffix(name, ".jsonl") {
			continue
		}
		path := filepath.Join(dir, name)
		info, err := os.Stat(path) // follow symlinks, matching os.Stat in readNew
		if err != nil || info.IsDir() {
			continue
		}
		candidates = append(candidates, candidate{
			path: path,
			mod:  info.ModTime(),
			name: name,
		})
	}
	if len(candidates) == 0 {
		return "", fmt.Errorf("no straggler_kpi_*.jsonl in %s", dir)
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].mod.Equal(candidates[j].mod) {
			return candidates[i].name < candidates[j].name
		}
		return candidates[i].mod.Before(candidates[j].mod)
	})
	return candidates[len(candidates)-1].path, nil
}

// readKPILines reads [offset, size), consumes only newline-terminated lines, and
// returns NPU samples (lines with a non-empty vals) plus the new offset. It never
// advances past the last newline, so a partially written line is retried later.
func readKPILines(path string, offset, size int64) ([]kpiShipSample, int64, error) {
	if size <= offset {
		return nil, offset, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, offset, err
	}
	defer f.Close()
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return nil, offset, err
	}

	end := size
	if end-offset > kpiReadChunk {
		end = offset + kpiReadChunk
	}
	buf := make([]byte, end-offset)
	n, err := io.ReadFull(f, buf)
	if err != nil && err != io.ErrUnexpectedEOF && err != io.EOF {
		return nil, offset, err
	}
	buf = buf[:n]
	lastNL := bytes.LastIndexByte(buf, '\n')
	if lastNL < 0 {
		return nil, offset, nil
	}
	complete := buf[:lastNL]
	lines := bytes.Split(complete, []byte{'\n'})
	samples := make([]kpiShipSample, 0, len(lines))
	for _, line := range lines {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		var sample kpiShipSample
		if err := json.Unmarshal(line, &sample); err != nil {
			continue
		}
		if len(sample.Vals) == 0 {
			continue // CPU-only line
		}
		samples = append(samples, sample)
	}
	return samples, offset + int64(lastNL) + 1, nil
}

// kpiShipper tails the KPI file and posts only NPU samples to the matched
// center. It is event-driven by file growth; there is no fixed POST period.
type kpiShipper struct {
	d         *Daemon
	tailer    *kpiTailer
	pending   []kpiShipSample
	business  string
	managed   bool
	lastErr   string
	lastErrAt time.Time
}

func (d *Daemon) kpiShipLoop(ctx context.Context) {
	if d.cfg.KpiDir == "" {
		return
	}
	s := &kpiShipper{
		d:      d,
		tailer: &kpiTailer{dir: d.cfg.KpiDir},
	}
	d.logf("KPI shipper enabled: dir=%s watch=%s", d.cfg.KpiDir, kpiWatchInterval)
	ticker := time.NewTicker(kpiWatchInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		center, key, business, daemon, ok := d.kpiShipTarget()
		if !ok {
			// Not matched: advance past new lines but discard them. On match we
			// start from the current tail instead of uploading a backlog.
			s.managed = false
			s.business = ""
			s.pending = s.pending[:0]
			if _, err := s.tailer.readNew(); err != nil {
				s.logErr("%v", err)
			}
			continue
		}
		if !s.managed {
			// First tick after matching: drop anything buffered before the match.
			s.managed = true
			s.business = business
			s.pending = s.pending[:0]
		} else if s.business != business {
			s.business = business
			s.pending = s.pending[:0]
		}
		samples, err := s.tailer.readNew()
		if err != nil {
			s.logErr("%v", err)
			continue
		}
		if len(samples) > 0 {
			s.pending = append(s.pending, samples...)
			if len(s.pending) > kpiMaxPending {
				s.pending = s.pending[len(s.pending)-kpiMaxPending:]
			}
		}
		if len(s.pending) == 0 {
			continue
		}
		n := len(s.pending)
		if n > kpiBatchSize {
			n = kpiBatchSize
		}
		if err := s.post(center, key, business, daemon, s.pending[:n]); err != nil {
			s.logErr("%v", err)
			continue
		}
		s.pending = s.pending[n:]
	}
}

func (d *Daemon) kpiShipTarget() (center, key, business, daemon string, ok bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.state != "managed" || d.centerAddr == "" || d.matchKey == "" || d.business == "" || d.daemonID == "" {
		return "", "", "", "", false
	}
	return d.centerAddr, d.matchKey, d.business, d.daemonID, true
}

func (s *kpiShipper) post(center, key, business, daemon string, samples []kpiShipSample) error {
	node, _ := os.Hostname()
	payload, err := json.Marshal(kpiShipReport{
		Node:    node,
		SentAt:  time.Now().Unix(),
		Samples: samples,
	})
	if err != nil {
		return fmt.Errorf("marshal report: %w", err)
	}
	target := strings.TrimRight(center, "/") + "/center/kpi/" +
		url.PathEscape(business) + "/" + url.PathEscape(daemon)
	req, err := http.NewRequest(http.MethodPost, target, bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Match-Key", key)
	client := &http.Client{Timeout: kpiPostTimeout}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
		return fmt.Errorf("center returned %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return nil
}

func (s *kpiShipper) logErr(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	now := time.Now()
	if msg == s.lastErr && now.Sub(s.lastErrAt) < kpiErrLogEvery {
		return
	}
	s.lastErr = msg
	s.lastErrAt = now
	s.d.logf("KPI ship: %s", msg)
}
