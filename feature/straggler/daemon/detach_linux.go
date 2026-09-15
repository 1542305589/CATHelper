//go:build linux

package daemon

import (
	"os/exec"
	"syscall"
)

// detachProcess puts the dynolog child in its own session/process group so that
// a terminal SIGINT / Ctrl-C aimed at the daemon's process group does not also
// kill dynolog — it must outlive the daemon.
func detachProcess(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}