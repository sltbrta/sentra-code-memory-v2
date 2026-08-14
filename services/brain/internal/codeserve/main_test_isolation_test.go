package codeserve_test

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// TestMain isolates every test in this package from the host's git config:
// the local hooks installer reads the effective core.hooksPath across all
// scopes, so a global hooksPath on the developer machine must not leak into
// test repositories.
func TestMain(m *testing.M) {
	tmp, err := os.MkdirTemp("", "sentra-codeserve-gitconfig-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(tmp, "gitconfig"))
	os.Setenv("GIT_CONFIG_SYSTEM", "/dev/null")
	code := m.Run()
	os.RemoveAll(tmp)
	os.Exit(code)
}
