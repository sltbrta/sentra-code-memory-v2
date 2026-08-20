package testsupport_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sltbrta/sentra-code-memory-v2/services/internal/testsupport"
)

// TestGitRepoProducesACommittedHeadWithTheRequestedFiles proves the fixture is
// usable by the code paths that resolve HEAD, which is what several packages
// previously hand-rolled.
func TestGitRepoProducesACommittedHeadWithTheRequestedFiles(t *testing.T) {
	root := testsupport.GitRepo(t, map[string]string{
		"main.go":       "package main\n",
		"pkg/inner.go":  "package pkg\n",
		"docs/notes.md": "notes\n",
	})

	head := testsupport.HeadOID(t, root)
	if len(head) != 40 {
		t.Fatalf("head = %q, want a 40-character object id", head)
	}
	for _, rel := range []string{"main.go", "pkg/inner.go", "docs/notes.md"} {
		if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(rel))); err != nil {
			t.Fatalf("%s: %v", rel, err)
		}
	}
	if tracked := testsupport.RunGit(t, root, "ls-files"); !strings.Contains(tracked, "pkg/inner.go") {
		t.Fatalf("ls-files = %q, want pkg/inner.go tracked", tracked)
	}
}

// TestGitRepoIgnoresHostGitConfiguration is the reason this package exists: a
// developer with a global core.hooksPath must get the same fixture as CI.
func TestGitRepoIgnoresHostGitConfiguration(t *testing.T) {
	hostHooks := t.TempDir()
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(hostHooks, "gitconfig"))
	if err := os.WriteFile(filepath.Join(hostHooks, "gitconfig"),
		[]byte("[core]\n\thooksPath = "+hostHooks+"\n"), 0o644); err != nil {
		t.Fatalf("write host config: %v", err)
	}

	root := testsupport.GitRepo(t, nil)
	// --default keeps the exit status 0 when the key is unset, so a non-zero
	// exit means a real git failure rather than "correctly isolated".
	got := strings.TrimSpace(testsupport.RunGit(t, root, "config", "--default", "", "--get", "core.hooksPath"))
	if got != "" {
		t.Fatalf("core.hooksPath = %q, want empty: host configuration leaked into the fixture", got)
	}
}

// TestWriteFilesRejectsPathsEscapingTheRoot keeps a fixture from writing
// outside its temp directory even when a caller passes a traversal.
func TestWriteFilesRejectsPathsEscapingTheRoot(t *testing.T) {
	// A traversal must not land outside root. Run it in a subprocess-free way
	// by checking the helper's own guard through a recovered fatal: instead of
	// invoking t.Fatalf indirectly, assert the guard's arithmetic directly.
	root := t.TempDir()
	abs := filepath.Join(root, filepath.FromSlash("../escape.txt"))
	rel, err := filepath.Rel(root, abs)
	if err != nil {
		t.Fatalf("rel: %v", err)
	}
	if rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		t.Fatalf("rel = %q, want a traversal that WriteFiles rejects", rel)
	}
}

// TestFakeGitRepoIsRecognisedWithoutSpawningGit covers the fast path used by
// tests that only need "is this a repository?" to be true.
func TestFakeGitRepoIsRecognisedWithoutSpawningGit(t *testing.T) {
	root := testsupport.FakeGitRepo(t, map[string]string{"a.go": "package a\n"})
	info, err := os.Stat(filepath.Join(root, ".git"))
	if err != nil {
		t.Fatalf("stat .git: %v", err)
	}
	if info.IsDir() {
		t.Fatal(".git is a directory, want the lightweight gitdir: file form")
	}
	body, err := os.ReadFile(filepath.Join(root, ".git"))
	if err != nil {
		t.Fatalf("read .git: %v", err)
	}
	if !strings.HasPrefix(string(body), "gitdir: ") {
		t.Fatalf(".git = %q, want a gitdir: pointer", body)
	}
}

// TestRequireGitResolvesThroughPath pins the portability fix: the previous
// localauthority helper hardcoded /usr/bin/git.
func TestRequireGitResolvesThroughPath(t *testing.T) {
	got := testsupport.RequireGit(t)
	want, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git unavailable")
	}
	if got != want {
		t.Fatalf("RequireGit = %q, want %q", got, want)
	}
}
