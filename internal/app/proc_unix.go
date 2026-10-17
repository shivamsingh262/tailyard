//go:build !windows

package app

import (
	"os/exec"
	"syscall"
)

// setProcGroup starts cmd in its own process group, so signalGroup reaches
// the children a shell or package runner spawns too.
func setProcGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

func signalGroup(cmd *exec.Cmd, kill bool) {
	if cmd.Process == nil {
		return
	}
	sig := syscall.SIGTERM
	if kill {
		sig = syscall.SIGKILL
	}
	_ = syscall.Kill(-cmd.Process.Pid, sig)
}

func shellCommand(s string) *exec.Cmd {
	return exec.Command("sh", "-c", s)
}
