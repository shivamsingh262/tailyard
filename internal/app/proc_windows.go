//go:build windows

package app

import (
	"os/exec"
	"strconv"
)

func setProcGroup(cmd *exec.Cmd) {}

// signalGroup ends the process tree: Windows has no SIGTERM, so a graceful
// stop and a kill are the same.
func signalGroup(cmd *exec.Cmd, kill bool) {
	if cmd.Process == nil {
		return
	}
	_ = exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(cmd.Process.Pid)).Run()
}

func shellCommand(s string) *exec.Cmd {
	return exec.Command("cmd", "/C", s)
}
