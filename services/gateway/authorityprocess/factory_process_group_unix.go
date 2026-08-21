//go:build unix

package authorityprocess

import (
	"os/exec"
	"syscall"
)

// configureFactoryProcessGroup puts the toolchain child in its own process
// group so the whole tree can be signalled together. This mirrors the
// change-set verification gate; the two are kept separate rather than shared
// because they live in different modules' internal trees.
func configureFactoryProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// killFactoryProcessGroup signals the child's entire process group.
// Signalling only the direct child -- what exec.CommandContext does by default
// -- leaves `go test`'s per-package binaries running past the deadline, still
// holding the overlay directory open.
func killFactoryProcessGroup(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
		return cmd.Process.Kill()
	}
	return nil
}
