//go:build unix

package sshx

import (
	"os/exec"
	"syscall"
)

// setProcessGroup puts the child in its own process group so that killing it
// also kills anything it spawned, and so terminal signals aimed at dol (e.g.
// Ctrl-C) do not reach it before dol had a chance to clean up.
func setProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

func killProcess(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	_ = cmd.Process.Kill()
}
