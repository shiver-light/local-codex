//go:build unix

package tools

import (
	"os/exec"
	"syscall"
)

// setProcGroup starts the command in its own process group so the whole
// tree (including backgrounded grandchildren) can be signalled at once.
func setProcGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// killProcessGroup SIGKILLs the command's entire process group. A negative
// PID targets the group whose ID is the child process's PID.
func killProcessGroup(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
}
