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

// TestMain isolates CLI hook tests from a developer machine's global Git
// configuration, especially a global core.hooksPath.
func TestMain(m *testing.M) {
	tmp, err := os.MkdirTemp("", "sentra-cli-gitconfig-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	_ = os.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(tmp, "gitconfig"))
	_ = os.Setenv("GIT_CONFIG_SYSTEM", "/dev/null")
	code := m.Run()
	_ = os.RemoveAll(tmp)
	os.Exit(code)
}

// cliGitRepo creates a throwaway repository for hooksPath restoration tests.
func cliGitRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	env := append([]string(nil), os.Environ()...)
	env = append(env, "HOME="+dir, "LANG=C", "GIT_CONFIG_GLOBAL="+filepath.Join(dir, "gitconfig"), "GIT_CONFIG_SYSTEM=/dev/null")
	cmd := exec.Command("git", "init", "-b", "main", dir)
	cmd.Env = env
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v (%s)", err, out)
	}
	cmd = exec.Command("git", "-C", dir, "-c", "user.email=test@example.invalid", "-c", "user.name=test", "commit", "--allow-empty", "-m", "init")
	cmd.Env = env
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git commit: %v (%s)", err, out)
	}
	return dir
}

// runCLI is the shared CLI-level harness: it executes one command line and
// decodes the single JSON response.
func runCLI(t *testing.T, args ...string) map[string]any {
	t.Helper()
	var stdout, stderr bytes.Buffer
	if code := execute(args, bytes.NewReader(nil), &stdout, &stderr); code != 0 {
		t.Fatalf("%v exit=%d stderr=%s", args, code, stderr.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &resp); err != nil {
		t.Fatalf("%v did not emit JSON: %v (%s)", args, err, stdout.String())
	}
	return resp
}

// TestHooksCLILifecycle exercises install → status → uninstall through the
// direct CLI against a temp root with the default repo-hooks strategy.
func TestHooksCLILifecycle(t *testing.T) {
	root := t.TempDir()

	install := runCLI(t, "hooks", "install", "--root", root)
	if install["ok"] != true {
		t.Fatalf("install: %+v", install)
	}
	if _, err := os.Stat(filepath.Join(root, ".sentra", "hooks")); err != nil {
		t.Fatalf("hooks dir missing after install: %v", err)
	}

	// Flags after the action must also parse (extractAction contract).
	status := runCLI(t, "hooks", "--root", root, "status")
	if status["ok"] != true {
		t.Fatalf("status: %+v", status)
	}
	installed, _ := status["installed"].([]any)
	if len(installed) == 0 {
		t.Fatalf("status reports no installed hooks: %+v", status)
	}

	uninstall := runCLI(t, "hooks", "uninstall", "--root", root)
	if uninstall["ok"] != true {
		t.Fatalf("uninstall: %+v", uninstall)
	}
	statusAfter := runCLI(t, "hooks", "status", "--root", root)
	installedAfter, _ := statusAfter["installed"].([]any)
	if len(installedAfter) != 0 {
		t.Fatalf("hooks still installed after uninstall: %+v", statusAfter)
	}
}

// TestHooksCLIRequiresAction rejects a bare hooks invocation.
func TestHooksCLIRequiresAction(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := execute([]string{"hooks"}, bytes.NewReader(nil), &stdout, &stderr); code != 2 {
		t.Fatalf("expected exit 2, got %d (%s)", code, stdout.String())
	}
}

// TestHooksCLIArbitraryCliPathAndRoot preserves the direct CLI trust
// model (issue #63): the operator-driven CLI is the only surface that
// may install lifecycle hooks with an arbitrary cli_path or root. The
// HTTP and MCP adapters are required to refuse the same payload; this
// test is the negative proof that the CLI continues to accept it.
// Without this guard, a future regression that pushed the gate down to
// codeserve.Handle itself would slip by undetected because the CLI
// tests would still pass on read paths.
func TestHooksCLIArbitraryCliPathAndRoot(t *testing.T) {
	root := cliGitRepo(t)
	// cli_path is the most security-relevant knob: the installed hook
	// scripts shell out to whatever binary this names. Picking a path
	// outside the repo would otherwise be the easiest escape hatch.
	cliPath := filepath.Join(t.TempDir(), "fake-binary")
	if err := os.WriteFile(cliPath, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	install := runCLI(t, "hooks", "install",
		"--root", root,
		"--cli-path", cliPath,
		"--strategy", "repo-hooks")
	if install["ok"] != true {
		t.Fatalf("direct CLI install with arbitrary cli_path must succeed (no gate): %+v", install)
	}

	status := runCLI(t, "hooks", "status", "--root", root)
	if status["ok"] != true {
		t.Fatalf("status after install: %+v", status)
	}
	installed, _ := status["installed"].([]any)
	if len(installed) == 0 {
		t.Fatalf("status reports no installed hooks: %+v", status)
	}

	// The hook script body must reference the operator-supplied cli_path,
	// proving the value flowed from the JSONL request map through
	// codeserve.Handle into lifecycle.Options without being sanitized
	// away. We read one of the four installed hooks.
	body, err := os.ReadFile(filepath.Join(root, ".sentra", "hooks", "post-commit"))
	if err != nil {
		t.Fatalf("post-commit hook should exist: %v", err)
	}
	if !strings.Contains(string(body), cliPath) {
		t.Fatalf("post-commit hook does not reference --cli-path (%q):\n%s",
			cliPath, string(body))
	}

	uninstall := runCLI(t, "hooks", "uninstall", "--root", root)
	if uninstall["ok"] != true {
		t.Fatalf("uninstall: %+v", uninstall)
	}
}

// TestHooksCLIArbitraryRootSameBoundary further pins the trust model
// (issue #63): the CLI accepts an arbitrary root regardless of where it
// points, as long as the operator chose it interactively. The MCP/HTTP
// gate is a property of the surface, not a path allow-list; this test
// makes that explicit so a future reviewer does not try to "fix" the
// CLI by adding a root allow-list. The repo-hooks strategy does not
// require a git repository (lifecycle.Install writes under
// <root>/.sentra/hooks unconditionally), so this install must
// succeed even though the directory is plain.
func TestHooksCLIArbitraryRootSameBoundary(t *testing.T) {
	root := t.TempDir() // a plain directory, NOT a git repo
	if err := os.WriteFile(filepath.Join(root, "README.md"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	got := runCLI(t, "hooks", "install", "--root", root,
		"--strategy", "repo-hooks")
	if got["ok"] != true {
		t.Fatalf("CLI must accept an arbitrary non-git root (no path allow-list): %+v", got)
	}
	defer runCLI(t, "hooks", "uninstall", "--root", root) //nolint:errcheck
	if _, err := os.Stat(filepath.Join(root, ".sentra", "hooks")); err != nil {
		t.Fatalf("hooks dir should exist after install: %v", err)
	}
}

// TestDenseLocalCLISearch runs the lexical arm through the CLI over a tiny
// temp corpus.
func TestDenseLocalCLISearch(t *testing.T) {
	root := t.TempDir()
	src := "package billing\nfunc InvoiceTotal() int { return 42 }\n"
	if err := os.WriteFile(filepath.Join(root, "billing.go"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	got := runCLI(t, "dense-local", "--root", root, "--q", "billing invoice")
	if got["ok"] != true {
		t.Fatalf("dense-local: %+v", got)
	}
	if got["route"] != "lexical" {
		t.Fatalf("route = %v, want lexical", got["route"])
	}
	raw, _ := json.Marshal(got["hits"])
	if !strings.Contains(string(raw), "billing.go") {
		t.Fatalf("hits missing billing.go: %s", raw)
	}
}

// TestDenseLocalCLIHardBounds proves request-controlled ceilings cannot
// exceed the hard defaults: top-k 999 clamps to 50 and max-corpus 999999
// clamps to 8192, and the receipt reports the enforced envelope.
func TestDenseLocalCLIHardBounds(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.go"), []byte("package a\nfunc Alpha() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got := runCLI(t, "dense-local", "--root", root, "--q", "alpha",
		"--top-k", "999", "--max-corpus", "999999")
	if got["ok"] != true {
		t.Fatalf("dense-local: %+v", got)
	}
	bounded, _ := got["bounded_by"].(map[string]any)
	if bounded["max_top_k"] != float64(50) {
		t.Fatalf("max_top_k not clamped to hard default: %+v", bounded)
	}
	if bounded["max_corpus"] != float64(8192) {
		t.Fatalf("max_corpus not clamped to hard default: %+v", bounded)
	}
	hits, _ := got["hits"].([]any)
	if len(hits) > 50 {
		t.Fatalf("more hits than the hard top-k ceiling: %d", len(hits))
	}
}

// TestDenseLocalCLIRejectsBadBounds refuses non-positive bound overrides.
func TestDenseLocalCLIRejectsBadBounds(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.go"), []byte("package a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got := runCLI(t, "dense-local", "--root", root, "--q", "a", "--max-corpus", "-3")
	if got["ok"] != false {
		t.Fatalf("expected bounds refusal, got %+v", got)
	}
}

// TestCLIHooksPreExistingHooksPathRestored asserts that install/uninstall
// preserves a user's existing local core.hooksPath and hook file.
func TestCLIHooksPreExistingHooksPathRestored(t *testing.T) {
	dir := cliGitRepo(t)
	env := append([]string(nil), os.Environ()...)
	env = append(env, "HOME="+dir, "LANG=C", "GIT_CONFIG_GLOBAL="+filepath.Join(dir, "gitconfig"), "GIT_CONFIG_SYSTEM=/dev/null")
	custom := filepath.Join(dir, ".git", "custom-hooks")
	if err := os.MkdirAll(custom, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(custom, "pre-commit"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("git", "-C", dir, "config", "--local", "core.hooksPath", ".git/custom-hooks")
	cmd.Env = env
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("set hooksPath: %v (%s)", err, out)
	}
	if got := runCLI(t, "hooks", "install", "--root", dir); got["ok"] != true {
		t.Fatalf("install: %+v", got)
	}
	if got := runCLI(t, "hooks", "uninstall", "--root", dir); got["ok"] != true {
		t.Fatalf("uninstall: %+v", got)
	}
	cmd = exec.Command("git", "-C", dir, "config", "--local", "--get", "core.hooksPath")
	cmd.Env = env
	out, err := cmd.Output()
	if err != nil || strings.TrimSpace(string(out)) != ".git/custom-hooks" {
		t.Fatalf("core.hooksPath not restored: %q (%v)", strings.TrimSpace(string(out)), err)
	}
	if _, err := os.Stat(filepath.Join(custom, "pre-commit")); err != nil {
		t.Fatalf("pre-existing hook disappeared: %v", err)
	}
}

// TestDenseLocalCLIRequiresQueryAndRoot fails fast on missing required flags.
func TestDenseLocalCLIRequiresQueryAndRoot(t *testing.T) {
	noQ := runCLI(t, "dense-local", "--root", t.TempDir())
	if noQ["ok"] != false {
		t.Fatalf("missing q should fail: %+v", noQ)
	}
	noRoot := runCLI(t, "dense-local", "--q", "billing")
	if noRoot["ok"] != false {
		t.Fatalf("missing root should fail: %+v", noRoot)
	}
}
