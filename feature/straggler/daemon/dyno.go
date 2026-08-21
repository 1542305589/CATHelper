package daemon

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
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

// locateLatestDumpDir finds the profiler dump directory created after `since`:
// the newest directory directly under ProfilerDir. dyno's response does not
// include the dump path; this heuristic stands until the dump layout is
// confirmed during first field testing.
func (d *Daemon) locateLatestDumpDir(since time.Time) (string, error) {
	entries, err := os.ReadDir(d.cfg.ProfilerDir)
	if err != nil {
		return "", fmt.Errorf("scan profiler dir: %w", err)
	}
	var candidates []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		info, ierr := e.Info()
		if ierr != nil {
			continue
		}
		if !info.ModTime().After(since) {
			continue
		}
		candidates = append(candidates, filepath.Join(d.cfg.ProfilerDir, e.Name()))
	}
	if len(candidates) == 0 {
		return "", fmt.Errorf("no new dump dir under %s after %s", d.cfg.ProfilerDir, since.Format(time.RFC3339))
	}
	sort.Slice(candidates, func(i, j int) bool {
		return fileModTime(candidates[i]).After(fileModTime(candidates[j]))
	})
	return candidates[0], nil
}

func fileModTime(path string) time.Time {
	info, err := os.Stat(path)
	if err != nil {
		return time.Time{}
	}
	return info.ModTime()
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
