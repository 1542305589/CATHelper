package daemon

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// dynologCmdPattern is the exact command line used to identify a running
// dynolog collector (grep via `ps -ef`).
const dynologCmdPattern = "dynolog --enable-ipc-monitor --certs-dir NO_CERTS"

// startDynolog spawns the dynolog collector subprocess (once, at daemon start)
// on a free port (via -port) so a fresh instance never clashes with a lingering
// one. The child is deliberately NOT killed on daemon shutdown — it keeps
// collecting so the user can still gather data after closing the daemon.
// Returns the *exec.Cmd to hold for reference, or nil when reusing an existing
// instance.
func startDynolog(bin string, logf func(format string, args ...any)) *exec.Cmd {
	port := findFreePort()
	args := []string{"--enable-ipc-monitor", "--certs-dir", "NO_CERTS"}
	if port > 0 {
		args = append(args, "-port", strconv.Itoa(port))
	}
	cmd := exec.Command(bin, args...)
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
	logf("dynolog started (pid %d, port %d)", cmd.Process.Pid, port)
	return cmd
}

// findFreePort asks the OS for a free TCP port and releases it. There is a tiny
// races window before dynolog binds it, acceptable here (collision just means
// the next start retries on a different port).
func findFreePort() int {
	ln, err := net.Listen("tcp", ":0")
	if err != nil {
		return 0
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

// dynologRunning reports whether a dynolog collector (matched by command line)
// is already running, via `ps -ef`.
func dynologRunning() bool {
	out, err := exec.Command("ps", "-ef").Output()
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(out), "\n") {
		if strings.Contains(line, dynologCmdPattern) {
			return true
		}
	}
	return false
}

// killDynolog terminates a running dynolog collector matched by command line.
func killDynolog() error {
	return exec.Command("pkill", "-f", dynologCmdPattern).Run()
}

// askKillDynolog prompts the user (stdin) whether to kill a running dynolog and
// start a fresh one; defaults to "no".
func askKillDynolog(logf func(format string, args ...any)) bool {
	fmt.Fprintf(os.Stderr, "[DAEMON] 检测到正在运行的 dynolog，是否 kill 并重新启动一个？[y/N]: ")
	reader := bufio.NewReader(os.Stdin)
	line, _ := reader.ReadString('\n')
	line = strings.ToLower(strings.TrimSpace(line))
	return line == "y" || line == "yes"
}

// ensureDynolog starts or reuses the dynolog collector at daemon startup. With
// a running instance it asks the user whether to kill-and-restart; otherwise it
// reuses the existing one.
func (d *Daemon) ensureDynolog() {
	if d.cfg.DynologBin == "" {
		return
	}
	if dynologRunning() {
		if askKillDynolog(d.logf) {
			if err := killDynolog(); err != nil {
				d.logf("kill existing dynolog: %v", err)
			} else {
				d.logf("killed existing dynolog")
			}
			d.dynolog = startDynolog(d.cfg.DynologBin, d.logf)
		} else {
			d.logf("reusing existing dynolog instance")
		}
		return
	}
	d.dynolog = startDynolog(d.cfg.DynologBin, d.logf)
}

// triggerCollection runs the dyno nputrace command that starts profiler capture
// on the matched vllm processes, and verifies it took effect via commandStatus.
func (d *Daemon) triggerCollection() error {
	args := []string{
		"--certs-dir", "NO_CERTS",
		"nputrace",
		"--start-step", "-1",
		"--iterations", strconv.Itoa(d.cfg.Iterations),
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

// noDBsErr builds the "no .db after analyse" error with a listing of the
// profiler root's top-level entries, so the on-disk layout can be validated
// during field testing (e.g. whether dyno wrote one subdir per rank).
func (d *Daemon) noDBsErr() error {
	var b strings.Builder
	entries, err := os.ReadDir(d.cfg.ProfilerDir)
	if err != nil {
		return fmt.Errorf("no ascend_pytorch_profiler_*.db found after analyse (scan %s failed: %v)", d.cfg.ProfilerDir, err)
	}
	for _, e := range entries {
		info, ierr := e.Info()
		if ierr != nil {
			continue
		}
		fmt.Fprintf(&b, "\n  %s  %s", info.ModTime().Format(time.RFC3339), e.Name())
	}
	return fmt.Errorf("no ascend_pytorch_profiler_*.db found after analyse under %s (top-level:%s)",
		d.cfg.ProfilerDir, b.String())
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
