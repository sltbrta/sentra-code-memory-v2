package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestMain isolates the CLI tests from the host's git config: the hooks
// installer reads the effective core.hooksPath across all scopes, so a
// global hooksPath on the developer machine must not leak into test repos.
func TestMain(m *testing.M) {
	tmp, err := os.MkdirTemp("", "sentra-cli-gitconfig-")
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

// cliGitRepo initializes a throwaway git repository for CLI hooks tests.
func cliGitRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git unavailable")
	}
	dir := t.TempDir()
	env := []string{
		"HOME=" + dir, "LANG=C", "PATH=" + os.Getenv("PATH"),
		"GIT_CONFIG_GLOBAL=" + filepath.Join(dir, "gitconfig"),
		"GIT_CONFIG_SYSTEM=/dev/null",
	}
	run := func(args ...string) {
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = env
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v (%s)", args, err, out)
		}
	}
	cmd := exec.Command("git", "init", "-b", "main", dir)
	cmd.Env = env
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v (%s)", err, out)
	}
	run("-c", "user.email=test@example.invalid", "-c", "user.name=test",
		"commit", "--allow-empty", "-m", "init")
	return dir
}

func runHooksCLI(t *testing.T, args ...string) map[string]any {
	t.Helper()
	var out, errOut bytes.Buffer
	if code := execute(append([]string{"hooks"}, args...), nil, &out, &errOut); code != 0 {
		t.Fatalf("hooks %v exit=%d stderr=%s", args, code, errOut.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(out.Bytes(), &resp); err != nil {
		t.Fatalf("hooks %v invalid JSON: %v\n%s", args, err, out.String())
	}
	return resp
}

// TestCLIHooksLifecycleRoundTrip covers the documented CLI form end to end:
// install, status, uninstall, with core.hooksPath cleared afterwards.
func TestCLIHooksLifecycleRoundTrip(t *testing.T) {
	dir := cliGitRepo(t)

	install := runHooksCLI(t, "install", "--root", dir, "--strategy", "repo-hooks")
	if install["ok"] != true {
		t.Fatalf("install: %+v", install)
	}
	status := runHooksCLI(t, "status", "--root", dir)
	if status["ok"] != true {
		t.Fatalf("status: %+v", status)
	}
	raw, _ := json.Marshal(status["installed"])
	for _, kind := range []string{"post-commit", "post-checkout", "post-merge", "pre-push"} {
		if !strings.Contains(string(raw), kind) {
			t.Fatalf("status missing %s: %s", kind, raw)
		}
	}
	uninstall := runHooksCLI(t, "uninstall", "--root", dir)
	if uninstall["ok"] != true {
		t.Fatalf("uninstall: %+v", uninstall)
	}

	cmd := exec.Command("git", "-C", dir, "config", "--local", "--get", "core.hooksPath")
	cmd.Env = []string{"HOME=" + dir, "LANG=C", "PATH=" + os.Getenv("PATH"),
		"GIT_CONFIG_GLOBAL=" + filepath.Join(dir, "gitconfig"),
		"GIT_CONFIG_SYSTEM=/dev/null"}
	if out, err := cmd.Output(); err == nil {
		t.Fatalf("core.hooksPath=%q should be unset after uninstall", strings.TrimSpace(string(out)))
	}
}

// TestCLIHooksPreExistingHooksPathRestored asserts the CLI path of the
// cold-review fix: a pre-existing core.hooksPath survives install and is
// restored verbatim by uninstall.
func TestCLIHooksPreExistingHooksPathRestored(t *testing.T) {
	dir := cliGitRepo(t)
	env := []string{
		"HOME=" + dir, "LANG=C", "PATH=" + os.Getenv("PATH"),
		"GIT_CONFIG_GLOBAL=" + filepath.Join(dir, "gitconfig"),
		"GIT_CONFIG_SYSTEM=/dev/null",
	}
	git := func(args ...string) {
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = env
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v (%s)", args, err, out)
		}
	}
	custom := filepath.Join(dir, ".git", "custom-hooks")
	if err := os.MkdirAll(custom, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(custom, "pre-commit"),
		[]byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	git("config", "--local", "core.hooksPath", ".git/custom-hooks")

	install := runHooksCLI(t, "install", "--root", dir)
	if install["ok"] != true {
		t.Fatalf("install: %+v", install)
	}
	uninstall := runHooksCLI(t, "uninstall", "--root", dir)
	if uninstall["ok"] != true {
		t.Fatalf("uninstall: %+v", uninstall)
	}

	cmd := exec.Command("git", "-C", dir, "config", "--local", "--get", "core.hooksPath")
	cmd.Env = env
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("core.hooksPath not restored: %v", err)
	}
	if got := strings.TrimSpace(string(out)); got != ".git/custom-hooks" {
		t.Fatalf("core.hooksPath=%q want %q", got, ".git/custom-hooks")
	}
	if _, err := os.Stat(filepath.Join(custom, "pre-commit")); err != nil {
		t.Fatalf("pre-existing hook file disappeared: %v", err)
	}
}
