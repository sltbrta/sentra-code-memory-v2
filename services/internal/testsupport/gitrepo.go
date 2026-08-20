// Package testsupport provides hermetic fixtures shared by tests across every
// service in this module.
//
// It exists because five packages had grown near-identical private copies of
// "make me a git repository in a temp dir" (codeserve, lifecycle, ingestion,
// broker/factory/runner, brain/localauthority), and they had drifted: one
// hardcoded /usr/bin/git instead of resolving it on PATH, and only some
// neutralised the developer's global git configuration. A test fixture that
// behaves differently per package is a source of false confidence.
//
// Everything here is deliberately importable from services/... rather than
// services/brain/internal/..., because the broker and gateway trees need it too.
package testsupport

import (
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// gitIdentity is applied per-invocation rather than written into a config file
// so a repository fixture never depends on committer identity resolution.
var gitIdentity = []string{
	"-c", "user.email=test@example.invalid",
	"-c", "user.name=sentra test",
	"-c", "commit.gpgsign=false",
	"-c", "init.defaultBranch=main",
}

// RequireGit skips the test when no git binary is on PATH and otherwise returns
// its resolved absolute path. Resolving through PATH (rather than assuming
// /usr/bin/git) is what makes the fixture portable across macOS, Linux, and
// nix-style environments.
func RequireGit(t *testing.T) string {
	t.Helper()
	path, err := exec.LookPath("git")
	if err != nil {
		t.Skip("testsupport: git unavailable on PATH")
	}
	return path
}

// IsolateGitConfig points the global and system git configuration at throwaway
// locations for the lifetime of the process, and returns a cleanup function.
//
// Call it from TestMain in any package whose code reads effective git config —
// core.hooksPath in particular resolves across all scopes, so a developer's
// global hooksPath would otherwise leak into every fixture repository and make
// results differ between a laptop and CI.
func IsolateGitConfig() (cleanup func()) {
	tmp, err := os.MkdirTemp("", "sentra-gitconfig-")
	if err != nil {
		// A TestMain has no *testing.T; failing loudly here beats running the
		// whole package against the host's real configuration.
		panic("testsupport: isolate git config: " + err.Error())
	}
	prevGlobal, hadGlobal := os.LookupEnv("GIT_CONFIG_GLOBAL")
	prevSystem, hadSystem := os.LookupEnv("GIT_CONFIG_SYSTEM")
	os.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(tmp, "gitconfig"))
	os.Setenv("GIT_CONFIG_SYSTEM", os.DevNull)
	return func() {
		restore("GIT_CONFIG_GLOBAL", prevGlobal, hadGlobal)
		restore("GIT_CONFIG_SYSTEM", prevSystem, hadSystem)
		os.RemoveAll(tmp)
	}
}

func restore(key, value string, had bool) {
	if had {
		os.Setenv(key, value)
		return
	}
	os.Unsetenv(key)
}

// GitEnv returns a minimal, hermetic environment for a git invocation rooted at
// dir. It carries PATH (git needs it to find its own subcommands) and nothing
// else that could vary between machines.
func GitEnv(dir string) []string {
	return []string{
		"HOME=" + dir,
		"LANG=C",
		"LC_ALL=C",
		"TZ=UTC",
		"PATH=" + os.Getenv("PATH"),
		"GIT_CONFIG_GLOBAL=" + filepath.Join(dir, ".gitconfig-fixture"),
		"GIT_CONFIG_SYSTEM=" + os.DevNull,
		// Never let a fixture repository run the developer's hooks.
		"GIT_TERMINAL_PROMPT=0",
	}
}

// RunGit executes git in root and returns its combined output, failing the test
// on a non-zero exit. Identity flags are supplied so commits work in a
// configuration-free environment.
func RunGit(t *testing.T, root string, args ...string) string {
	t.Helper()
	git := RequireGit(t)
	full := append([]string{"-C", root}, append(append([]string{}, gitIdentity...), args...)...)
	cmd := exec.Command(git, full...)
	cmd.Env = GitEnv(root)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("testsupport: git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

// GitRepo creates a temp directory, initialises a git repository in it, writes
// files, and commits them. It returns the repository root.
//
// files maps repository-relative slash-separated paths to contents; parent
// directories are created. Passing nil yields a repository whose only commit
// contains a README, because several callers need a non-empty HEAD to resolve.
func GitRepo(t *testing.T, files map[string]string) string {
	t.Helper()
	RequireGit(t)
	root := t.TempDir()

	RunGit(t, root, "init", "-q")
	if len(files) == 0 {
		files = map[string]string{"README.md": "sentra test fixture\n"}
	}
	WriteFiles(t, root, files)
	RunGit(t, root, "add", "--all")
	RunGit(t, root, "commit", "-q", "-m", "init")
	return root
}

// WriteFiles writes files under root, creating parent directories. Paths are
// slash-separated and must stay inside root.
func WriteFiles(t *testing.T, root string, files map[string]string) {
	t.Helper()
	// Sorted so a failure reports the same path on every run.
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		abs := filepath.Join(root, filepath.FromSlash(name))
		rel, err := filepath.Rel(root, abs)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			t.Fatalf("testsupport: %q escapes the fixture root", name)
		}
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			t.Fatalf("testsupport: mkdir for %q: %v", name, err)
		}
		if err := os.WriteFile(abs, []byte(files[name]), 0o644); err != nil {
			t.Fatalf("testsupport: write %q: %v", name, err)
		}
	}
}

// HeadOID returns the current HEAD commit object id.
func HeadOID(t *testing.T, root string) string {
	t.Helper()
	return strings.TrimSpace(RunGit(t, root, "rev-parse", "HEAD"))
}

// WorkTree creates a temp directory containing files but no git repository. Use
// it for the many code paths that only need a source tree to crawl, so those
// tests neither require a git binary nor pay the cost of process spawns.
func WorkTree(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	WriteFiles(t, root, files)
	return root
}

// FakeGitRepo creates a work tree whose .git is a plain file of the "gitdir:"
// form. Code that only asks "is this a repository?" is satisfied, without
// spawning git at all. Do not use it for anything that runs a git command.
func FakeGitRepo(t *testing.T, files map[string]string) string {
	t.Helper()
	root := WorkTree(t, files)
	elsewhere := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, ".git"), []byte("gitdir: "+elsewhere+"\n"), 0o644); err != nil {
		t.Fatalf("testsupport: write fake .git: %v", err)
	}
	return root
}
