//go:build !unix

package workflow

import "os/exec"

// configureProcessGroup is a no-op where process groups are unavailable.
func configureProcessGroup(cmd *exec.Cmd) {}

// killProcessGroup falls back to killing the direct child.
func killProcessGroup(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	return cmd.Process.Kill()
}
