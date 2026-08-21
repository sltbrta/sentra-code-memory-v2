//go:build !unix

package authorityprocess

import "os/exec"

// configureFactoryProcessGroup is a no-op where process groups are unavailable.
func configureFactoryProcessGroup(cmd *exec.Cmd) {}

// killFactoryProcessGroup falls back to killing the direct child.
func killFactoryProcessGroup(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	return cmd.Process.Kill()
}
