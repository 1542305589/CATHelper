package daemon

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// startDynolog spawns the dynolog collector subprocess (once, at daemon start).
// When the IPC port is already taken the process exits quickly — the daemon
// logs and reuses the existing instance, since dyno talks to it over IPC
// anyway. Returns the *exec.Cmd to hold for cleanup, or nil when reusing.
func startDynolog(bin string, logf func(format string, args ...any)) *exec.Cmd {
	cmd := exec.Command(bin, "--enable-ipc-monitor", "--certs-dir", "NO_CERTS")
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		logf("dynolog start failed (%v) — reusing existing instance", err)
		return nil
	}
	go func() {
		if err := cmd.Wait(); err != nil {
			logf("dynolog exited: %v", err)
		}
	}()
	logf("dynolog started (pid %d)", cmd.Process.Pid)
	return cmd
}

// triggerCollection runs the dyno nputrace command that starts profiler capture
// on the matched vllm processes, and verifies it took effect via commandStatus.
func (d *Daemon) triggerCollection() error {
	args := []string{
		"--certs-dir", "NO_CERTS",
		"nputrace",
		"--start-step", "-1",
		"--iterations", "5",
		"--activities", "NPU,CPU",
		"--profiler-level", "Level0",
		"--msprof-tx",
		"--export-type", "Db",
		"--log-file", d.cfg.ProfilerDir,
	}
	out, err := exec.Command(d.cfg.DynoBin, args...).CombinedOutput()
	// The collection verdict comes only from commandStatus in the response:
	// dyno's process exit code says nothing about whether data was captured, so
	// an effective response wins even when the process exited non-zero.
	resp, perr := parseDynoResponse(string(out))
	if perr != nil {
		if err != nil {
			return fmt.Errorf("dyno trigger: %v (stderr: %s)", err, strings.TrimSpace(string(out)))
		}
		return fmt.Errorf("dyno response: %v (stdout: %s)", perr, strings.TrimSpace(string(out)))
	}
	if resp.CommandStatus != "effective" {
		return fmt.Errorf("dyno commandStatus=%s, processesMatched=%v", resp.CommandStatus, resp.ProcessesMatched)
	}
	if len(resp.ProcessesMatched) == 0 {
		return fmt.Errorf("dyno processesMatched empty — no vllm process with MSMONITOR_USE_DAEMON=1")
	}
	return nil
}

// parseDynoResponse extracts the JSON object from dyno's stdout. The payload is
// shaped "response = {...}" and is wrapped in a preamble ("Security Warning: ...",
// "NpuTrace config = ...") plus trailing status lines ("Matched N processes",
// "Trace output files will be written to: ..."). The JSON object itself is the
// text between the first '{' and the last '}' of the whole output.
func parseDynoResponse(stdout string) (*dynoResponse, error) {
	start := strings.Index(stdout, "{")
	end := strings.LastIndex(stdout, "}")
	if start >= 0 && end > start {
		var r dynoResponse
		if err := json.Unmarshal([]byte(stdout[start:end+1]), &r); err == nil {
			return &r, nil
		}
	}
	// Fallback: the whole trimmed output is JSON (no preamble/trailing text).
	var r dynoResponse
	if err := json.Unmarshal([]byte(strings.TrimSpace(stdout)), &r); err != nil {
		return nil, err
	}
	return &r, nil
}

// locateLatestDumpDir returns the newest dump directory directly under
// ProfilerDir. Trigger time is deliberately NOT used as a lower bound: the dump
// dirs are created by the profiler during collection initiation and can land a
// few seconds before the dyno client command returns, so an "mtime after
// trigger" filter is inherently racy. Cycles run one interval apart and nothing
// else writes into ProfilerDir, so the newest dir is the current cycle's
// output. It is only rejected as stale (older than a whole interval) when the
// collection produced nothing at all.
func (d *Daemon) locateLatestDumpDir() (string, error) {
	entries, err := os.ReadDir(d.cfg.ProfilerDir)
	if err != nil {
		return "", fmt.Errorf("scan profiler dir: %w", err)
	}
	best, bestT := "", time.Time{}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		info, ierr := e.Info()
		if ierr != nil {
			continue
		}
		if info.ModTime().After(bestT) {
			best, bestT = e.Name(), info.ModTime()
		}
	}
	if best == "" || time.Since(bestT) > d.cfg.Interval {
		return "", d.noNewDumpErr(bestT)
	}
	return filepath.Join(d.cfg.ProfilerDir, best), nil
}

// noNewDumpErr builds the "no fresh dump dir" error including a listing of what
// currently sits under ProfilerDir, so the on-disk layout can be validated
// during field testing.
func (d *Daemon) noNewDumpErr(newest time.Time) error {
	newestTxt := newest.Format(time.RFC3339)
	if newest.IsZero() {
		newestTxt = "none"
	}
	var b strings.Builder
	entries, err := os.ReadDir(d.cfg.ProfilerDir)
	if err != nil {
		return fmt.Errorf("no fresh dump dir under %s (scan failed: %v)", d.cfg.ProfilerDir, err)
	}
	for _, e := range entries {
		info, ierr := e.Info()
		if ierr != nil {
			continue
		}
		fmt.Fprintf(&b, "\n  %s  %s", info.ModTime().Format(time.RFC3339), e.Name())
	}
	return fmt.Errorf("no fresh dump dir under %s (newest=%s, top-level:%s)",
		d.cfg.ProfilerDir, newestTxt, b.String())
}

// runAnalyse converts the raw profiler dump into .db files via torch_npu's
// analyse. python (with torch_npu installed) must be on PATH.
func runAnalyse(profilerPath string, logf func(format string, args ...any)) error {
	code := fmt.Sprintf(
		"from torch_npu.profiler.profiler import analyse; analyse(profiler_path=%q, export_type=['db'])",
		profilerPath,
	)
	out, err := exec.Command("python", "-c", code).CombinedOutput()
	if err != nil {
		return fmt.Errorf("python analyse: %v (%s)", err, strings.TrimSpace(string(out)))
	}
	logf("python analyse done for %s", profilerPath)
	return nil
}

// findDBs walks dir recursively and returns all ascend_pytorch_profiler_*.db paths.
func findDBs(dir string) []string {
	var dbs []string
	_ = filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		base := filepath.Base(path)
		if strings.HasPrefix(base, "ascend_pytorch_profiler_") && strings.HasSuffix(base, ".db") {
			dbs = append(dbs, path)
		}
		return nil
	})
	return dbs
}
