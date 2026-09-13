//go:build !unix

package tools

import "os/exec"

// setProcGroup is a no-op on platforms without Unix process groups.
func setProcGroup(cmd *exec.Cmd) {}

// killProcessGroup falls back to killing only the direct child process.
func killProcessGroup(cmd *exec.Cmd) {
	if cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
}
