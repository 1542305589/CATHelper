//go:build !linux

package daemon

import "os/exec"

// detachProcess is a no-op on non-Linux platforms (no Setsid available).
func detachProcess(cmd *exec.Cmd) {}