//go:build unix

package workflow

import (
	"os/exec"
	"syscall"
)

// configureProcessGroup puts the verifier in its own process group so the whole
// tree can be signalled together.
func configureProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// killProcessGroup signals the verifier's entire process group. Signalling only
// the direct child (what exec.CommandContext does by default) leaves a test
// runner's workers alive past the deadline.
func killProcessGroup(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
		return cmd.Process.Kill()
	}
	return nil
}
