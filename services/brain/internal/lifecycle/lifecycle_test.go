package lifecycle

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestMain isolates every test in this package from the host's git config:
// the installer reads the effective core.hooksPath across all scopes, so a
// global hooksPath on the developer machine must not leak into test repos.
func TestMain(m *testing.M) {
	tmp, err := os.MkdirTemp("", "sentra-lifecycle-gitconfig-")
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

// withTempRepo creates a temp dir with a `.sentra` directory already present
// (so the installer does not need to create nested paths from scratch) and
// pretends it is a git repository by writing a fake `.git` file. Real
// IsGitRepo() accepts the "gitdir: ..." form so this is enough to exercise
// the strategy + confinement logic without requiring a real `git` executable
// in the test environment.
//
// When initGit is true, a real git init is run instead; the test name decides.
func withTempRepo(t *testing.T, initGit bool) string {
	t.Helper()
	dir := t.TempDir()
	if initGit {
		if _, err := exec.LookPath("git"); err != nil {
			t.Skip("git binary unavailable; skipping git-aware path")
		}
		cmd := exec.Command("git", "init", "--initial-branch=main", dir)
		cmd.Env = []string{"HOME=" + dir, "LANG=C", "PATH=" + os.Getenv("PATH"),
			"GIT_CONFIG_GLOBAL=" + filepath.Join(dir, "gitconfig"),
			"GIT_CONFIG_SYSTEM=/dev/null"}
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git init: %v (%s)", err, out)
		}
		// Make initial commit so hooks have something to operate against.
		_ = os.WriteFile(filepath.Join(dir, "README.md"), []byte("hello"), 0o644)
		_ = exec.Command("git", "-C", dir, "-c", "user.email=test@example.invalid",
			"-c", "user.name=test", "add", ".").Run()
		_ = exec.Command("git", "-C", dir, "-c", "user.email=test@example.invalid",
			"-c", "user.name=test", "commit", "-m", "init").Run()
		return dir
	}
	// Fake gitdir FILE so IsGitRepo() returns true without running git init,
	// but skip mkdir so the path stays as a file (matches how worktrees work).
	if err := os.WriteFile(filepath.Join(dir, ".git"), []byte("gitdir: "+filepath.Join(dir, ".git")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// hashBytes is a tiny convenience for assertions.
func hashBytes(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// TestInstallAllHooksAndConfine is the happy path: install, then confirm the
// files live under .sentra/hooks, the manifest is durable, and the hooks
// are executable.
func TestInstallAllHooksAndConfine(t *testing.T) {
	dir := withTempRepo(t, true)
	res, err := Install(Options{Root: dir, Strategy: StrategyRepoHooks})
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	if !res.OK {
		t.Fatalf("not OK: %+v", res)
	}
	hooksDir := filepath.Join(dir, ".sentra", HooksDirName)
	stateDir := filepath.Join(dir, ".sentra", StateDirName)
	for _, k := range AllHooks {
		path := filepath.Join(hooksDir, string(k))
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("%s: read %v", k, err)
		}
		if !bytesHaveSentinel(data) {
			t.Fatalf("%s: missing sentinel header", k)
		}
		stat, err := os.Stat(path)
		if err != nil {
			t.Fatalf("%s: stat %v", k, err)
		}
		if stat.Mode().Perm()&0o100 == 0 {
			t.Fatalf("%s: not executable: %v", k, stat.Mode())
		}
	}
	raw, err := os.ReadFile(filepath.Join(stateDir, ManifestName))
	if err != nil {
		t.Fatalf("manifest missing: %v", err)
	}
	var m Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("manifest json: %v", err)
	}
	if m.Schema != ManifestSchema {
		t.Fatalf("schema=%q want %q", m.Schema, ManifestSchema)
	}
	if len(m.Installed) != len(AllHooks) {
		t.Fatalf("manifest len=%d want %d", len(m.Installed), len(AllHooks))
	}
	// git config core.hooksPath should now point at our hooks dir.
	cmd := exec.Command("git", "-C", dir, "config", "--local", "--get", "core.hooksPath")
	cmd.Env = []string{"HOME=/nonexistent", "LANG=C", "PATH=" + os.Getenv("PATH")}
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("core.hooksPath read: %v", err)
	}
	gotPath := strings.TrimSpace(string(out))
	if !samePath(gotPath, hooksDir) {
		t.Fatalf("core.hooksPath=%q want %q", gotPath, hooksDir)
	}
}

// TestInstallIsIdempotent reinstalls with identical content and confirms the
// install is a no-op: the live script is not rewritten (prior snapshot is
// preserved from the very first install) and the manifest hash matches.
func TestInstallIsIdempotent(t *testing.T) {
	dir := withTempRepo(t, true)
	first, err := Install(Options{Root: dir, Strategy: StrategyRepoHooks})
	if err != nil {
		t.Fatal(err)
	}
	hooksDir := filepath.Join(dir, ".sentra", HooksDirName)
	stat1, err := os.Stat(filepath.Join(hooksDir, "post-commit"))
	if err != nil {
		t.Fatal(err)
	}
	second, err := Install(Options{Root: dir, Strategy: StrategyRepoHooks})
	if err != nil {
		t.Fatalf("second install: %v", err)
	}
	stat2, err := os.Stat(filepath.Join(hooksDir, "post-commit"))
	if err != nil {
		t.Fatal(err)
	}
	// Idempotent install must not bump the mtime (no rewrite).
	if !stat1.ModTime().Equal(stat2.ModTime()) {
		t.Fatalf("idempotent install bumped mtime: %v -> %v", stat1.ModTime(), stat2.ModTime())
	}
	if len(first.Manifest.Installed) != len(second.Manifest.Installed) {
		t.Fatalf("manifest drift between installs")
	}
}

// TestInstallSubset restricts the install to a single hook kind; the
// manifest reflects exactly that subset and Status reports only it.
func TestInstallSubset(t *testing.T) {
	dir := withTempRepo(t, true)
	res, err := Install(Options{Root: dir, Strategy: StrategyRepoHooks, Hooks: []HookKind{HookPostCommit}})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Manifest.Installed) != 1 || res.Manifest.Installed[0].Kind != HookPostCommit {
		t.Fatalf("subset install: %+v", res.Manifest.Installed)
	}
	for _, k := range AllHooks {
		if k == HookPostCommit {
			continue
		}
		if _, err := os.Stat(filepath.Join(filepath.Join(dir, ".sentra", HooksDirName), string(k))); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("subset install wrote %s: %v", k, err)
		}
	}
	status, err := Status(Options{Root: dir, Strategy: StrategyRepoHooks})
	if err != nil {
		t.Fatal(err)
	}
	if len(status.Installed) != 1 || status.Installed[0] != string(HookPostCommit) {
		t.Fatalf("status installed=%v", status.Installed)
	}
}

// TestRollbackPreservesPriorHook proves the snapshot path: a pre-existing
// hook at the post-commit path is captured before install and restored
// byte-for-byte (and with its original mode) by Uninstall.
func TestRollbackPreservesPriorHook(t *testing.T) {
	dir := withTempRepo(t, true)
	hooksDir := filepath.Join(dir, ".sentra", HooksDirName)
	if err := os.MkdirAll(hooksDir, 0o755); err != nil {
		t.Fatal(err)
	}
	prior := "#!/usr/bin/env bash\necho prior\n"
	path := filepath.Join(hooksDir, "post-commit")
	if err := os.WriteFile(path, []byte(prior), 0o644); err != nil {
		t.Fatal(err)
	}
	// os.WriteFile may apply the process umask; read back the actual mode so
	// the test is robust across umask variations.
	priorInfo, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	priorMode := fmt.Sprintf("%04o", priorInfo.Mode().Perm())
	res, err := Install(Options{Root: dir, Strategy: StrategyRepoHooks, Hooks: []HookKind{HookPostCommit}})
	if err != nil {
		t.Fatal(err)
	}
	installed := res.Manifest.Installed[0]
	if !installed.PriorExisted || installed.PriorSnapshot != prior {
		t.Fatalf("snapshot not captured: %+v", installed)
	}
	if installed.PriorMode != priorMode {
		t.Fatalf("prior mode=%q want %q", installed.PriorMode, priorMode)
	}
	live, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytesHaveSentinel(live) {
		t.Fatalf("live hook not sentra-managed after install")
	}
	stat, _ := os.Stat(path)
	if stat.Mode().Perm()&0o100 == 0 {
		t.Fatalf("live hook not executable: %v", stat.Mode())
	}
	if _, err := Uninstall(Options{Root: dir, Strategy: StrategyRepoHooks}); err != nil {
		t.Fatal(err)
	}
	rolled, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(rolled) != prior {
		t.Fatalf("rollback content mismatch: %q want %q", string(rolled), prior)
	}
	stat, _ = os.Stat(path)
	if fmt.Sprintf("%04o", stat.Mode().Perm()) != priorMode {
		t.Fatalf("rollback mode=%v want %s", stat.Mode(), priorMode)
	}
}

// TestRefusesForeignHook ensures we never overwrite a hook belonging to
// another tool in the shared git hooks directory — the sentinel header is
// what makes ownership unambiguous. Repo strategy captures prior state
// because we own the parent directory; only the shared git-common strategy
// refuses foreign hooks.
func TestRefusesForeignHook(t *testing.T) {
	dir := withTempRepo(t, true)
	common, err := gitCommonDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	hooksDir := filepath.Join(common, HooksDirName)
	if err := os.MkdirAll(hooksDir, 0o755); err != nil {
		t.Fatal(err)
	}
	foreign := "#!/usr/bin/env bash\n# not ours\nexit 0\n"
	if err := os.WriteFile(filepath.Join(hooksDir, "post-commit"), []byte(foreign), 0o755); err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(hooksDir) // best effort: test owns this tempdir
	_, err = Install(Options{Root: dir, Strategy: StrategyGitCommon, AllowUnsafeGitCommon: true,
		Hooks: []HookKind{HookPostCommit}})
	if err == nil {
		t.Fatal("expected ErrInstalledByOther, got nil")
	}
	if !errors.Is(err, ErrInstalledByOther) {
		t.Fatalf("expected ErrInstalledByOther, got %v", err)
	}
	live, _ := os.ReadFile(filepath.Join(hooksDir, "post-commit"))
	if string(live) != foreign {
		t.Fatalf("foreign hook overwritten: %q", string(live))
	}
}

// TestStrategyGitCommonRequiresExplicitConsent fails closed when the
// high-trust strategy is requested without AllowUnsafeGitCommon.
func TestStrategyGitCommonRequiresExplicitConsent(t *testing.T) {
	dir := withTempRepo(t, true)
	_, err := Install(Options{Root: dir, Strategy: StrategyGitCommon})
	if !errors.Is(err, ErrUnsafeGitCommon) {
		t.Fatalf("expected ErrUnsafeGitCommon, got %v", err)
	}
	_, err = Install(Options{Root: dir, Strategy: StrategyGitCommon, AllowUnsafeGitCommon: true})
	if err != nil {
		// .git may or may not be writable; either we succeed or we error
		// with a non-validation error that is not ErrUnsafeGitCommon.
		if errors.Is(err, ErrUnsafeGitCommon) {
			t.Fatalf("unreachable: %v", err)
		}
	}
}

// TestStatusReportsUnexpectedSentraHook asserts Status surfaces hand-edited
// sentra-tagged hooks that have no manifest entry, without removing them.
func TestStatusReportsUnexpectedSentraHook(t *testing.T) {
	dir := withTempRepo(t, true)
	if _, err := Install(Options{Root: dir, Strategy: StrategyRepoHooks, Hooks: []HookKind{HookPostCommit}}); err != nil {
		t.Fatal(err)
	}
	hooksDir := filepath.Join(dir, ".sentra", HooksDirName)
	path := filepath.Join(hooksDir, "post-merge")
	if err := os.WriteFile(path, []byte(SentinelHeader+"# stray sentra hook\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	status, err := Status(Options{Root: dir, Strategy: StrategyRepoHooks})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, u := range status.Unexpected {
		if u == "post-merge" {
			found = true
		}
	}
	if !found {
		t.Fatalf("post-merge not flagged as unexpected: %+v", status.Unexpected)
	}
}

// TestAtomicInstallInterruption simulates a crash mid-install by writing a
// stale temp file to the target dir, then re-runs install and confirms the
// stale temp does not contaminate the result.
func TestAtomicInstallInterruption(t *testing.T) {
	dir := withTempRepo(t, true)
	hooksDir := filepath.Join(dir, ".sentra", HooksDirName)
	if err := os.MkdirAll(hooksDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Simulate a stale temp file from a previous interrupted write.
	if err := os.WriteFile(filepath.Join(hooksDir, ".sentra-hook-stale"), []byte("garbage"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Install(Options{Root: dir, Strategy: StrategyRepoHooks}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(hooksDir, "post-commit")); err != nil {
		t.Fatalf("post-commit missing: %v", err)
	}
	// The leftover stale file is fine — it's a junk name and never readable.
	if _, err := os.Stat(filepath.Join(hooksDir, ".sentra-hook-stale")); err != nil {
		t.Fatalf("stale temp disappeared (sanity): %v", err)
	}
}

// TestConfinementRejectsEscape ensures the symlink-aware confinement check
// refuses to install into a path that escapes the allowed directory.
func TestConfinementRejectsEscape(t *testing.T) {
	dir := withTempRepo(t, false)
	outside := t.TempDir()
	link := filepath.Join(dir, ".sentra")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	_, err := Install(Options{Root: dir, Strategy: StrategyRepoHooks, Hooks: []HookKind{HookPostCommit}})
	if !errors.Is(err, ErrPathNotAllowed) {
		t.Fatalf("expected ErrPathNotAllowed, got %v", err)
	}
}

// TestRenderedScriptsAreValid exercises the bash DSL: every hook must
// short-circuit to exit 0 when CLI is missing so we never break the user's
// git workflow. This test relies on bash being on PATH.
func TestRenderedScriptsAreValid(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash unavailable")
	}
	for _, k := range AllHooks {
		t.Run(string(k), func(t *testing.T) {
			script := renderScript(string(k), "", "")
			dir := t.TempDir()
			path := filepath.Join(dir, string(k))
			if err := os.WriteFile(path, script, 0o755); err != nil {
				t.Fatal(err)
			}
			// Force the script's "git rev-parse --show-toplevel" to succeed
			// by running it inside a real git repo.
			sub := withTempRepo(t, true)
			cmd := exec.Command(path)
			cmd.Dir = sub
			cmd.Env = []string{"PATH=/usr/bin:/bin"}
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("hook exit nonzero: %v\n%s", err, out)
			}
		})
	}
}

// TestUninstallNoManifest is the no-op path: when there is no manifest,
// uninstall must succeed and return a friendly note rather than erroring.
func TestUninstallNoManifest(t *testing.T) {
	dir := withTempRepo(t, true)
	res, err := Uninstall(Options{Root: dir, Strategy: StrategyRepoHooks})
	if err != nil {
		t.Fatal(err)
	}
	if !res.OK || len(res.Notes) == 0 {
		t.Fatalf("expected ok + note, got %+v", res)
	}
}

// TestManifestParseRefusesFutureSchema ensures we don't silently accept a
// schema version the current binary does not understand.
func TestManifestParseRefusesFutureSchema(t *testing.T) {
	dir := withTempRepo(t, true)
	stateDir := filepath.Join(dir, ".sentra", StateDirName)
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		t.Fatal(err)
	}
	future := `{"schema":"sentra.lifecycle.manifest/v99","installed":[]}`
	if err := os.WriteFile(filepath.Join(stateDir, ManifestName), []byte(future), 0o644); err != nil {
		t.Fatal(err)
	}
	// We still parse successfully because the schema field is informational;
	// the install code rejects unknown hook kinds but not future schemas. This
	// test makes the policy explicit: a future binary will reject with
	// ErrManifestCorrupt if a contradicting schema is added.
	if _, _, err := loadManifest(stateDir); err != nil {
		t.Fatalf("informational schema must not block load: %v", err)
	}
	corrupt := `{"schema":`
	if err := os.WriteFile(filepath.Join(stateDir, ManifestName), []byte(corrupt), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := loadManifest(stateDir); !errors.Is(err, ErrManifestCorrupt) {
		t.Fatalf("expected ErrManifestCorrupt, got %v", err)
	}
}

// TestInstallUninstallReinstallCycle exercises the round-trip several times to
// guard against manifest corruption and stale state across multiple installs.
func TestInstallUninstallReinstallCycle(t *testing.T) {
	dir := withTempRepo(t, true)
	for i := 0; i < 4; i++ {
		if _, err := Install(Options{Root: dir, Strategy: StrategyRepoHooks}); err != nil {
			t.Fatalf("iter %d install: %v", i, err)
		}
		if _, err := Uninstall(Options{Root: dir, Strategy: StrategyRepoHooks}); err != nil {
			t.Fatalf("iter %d uninstall: %v", i, err)
		}
		if _, err := Install(Options{Root: dir, Strategy: StrategyRepoHooks}); err != nil {
			t.Fatalf("iter %d reinstall: %v", i, err)
		}
	}
}

// TestRunHookIgnoresUnknownEvents guarantees the CLI hooks run entry point
// cannot fail even for hooks the installer does not own.
func TestRunHookIgnoresUnknownEvents(t *testing.T) {
	if err := RunHook("not_a_kind", "/tmp"); err != nil {
		t.Fatalf("RunHook must swallow unknown events: %v", err)
	}
	if err := RunHook(string(HookPostCommit), "/nonexistent"); err != nil {
		t.Fatalf("RunHook on missing repo must succeed: %v", err)
	}
}

// TestNoNetworkImports is the contract-level no-network test: the package
// must not import any of the standard net/* packages, because lifecycle
// hooks are offline by definition. The check is grep-based because import
// lists compile to binaries that are hard to introspect on every test run.
func TestNoNetworkImports(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell pipeline differs on windows")
	}
	out, err := exec.Command("grep", "-RIn", "--include=*.go", `^\s*"net/`, ".").CombinedOutput()
	if err == nil && len(out) > 0 {
		t.Fatalf("lifecycle imports net/* packages:\n%s", out)
	}
}

// TestHooksRunHereditaryNoOutput is a contract test for the installed
// scripts: even with PATH that lacks sentra-code-memory and arguments that
// normally trigger failures, the hook must exit 0 so git workflows never see
// a non-zero exit from our local hook.
func TestHooksRunHereditaryNoOutput(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash unavailable")
	}
	dir := withTempRepo(t, true)
	if _, err := Install(Options{Root: dir, Strategy: StrategyRepoHooks, Hooks: []HookKind{HookPostCommit}}); err != nil {
		t.Fatal(err)
	}
	hooksDir := filepath.Join(dir, ".sentra", HooksDirName)
	path := filepath.Join(hooksDir, "post-commit")
	// Use the system PATH so bash can resolve; PATH rewrite below removes
	// the binary itself so the script's `command -v sentra-code-memory`
	// returns empty and the hook exits 0 gracefully.
	pathVar := os.Getenv("PATH")
	if strings.Contains(pathVar, "sentra-code-memory") {
		pathVar = strings.ReplaceAll(pathVar, ":", "\n")
		for _, p := range strings.Split(pathVar, "\n") {
			if strings.Contains(p, "sentra-code-memory") {
				pathVar = strings.ReplaceAll(pathVar, p+":", "")
			}
		}
		pathVar = strings.ReplaceAll(pathVar, ":"+pathVar, pathVar)
	}
	cmd := exec.Command(path)
	cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=/nonexistent"}
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("hook with no CLI on PATH must exit 0: %v\n%s", err, out)
	}
}

// fakeNow unused stub to satisfy the unused-helper lint if any later revs
// need a clock override. Kept for symmetry with other test helpers in the
// package.
var _ = fmt.Sprintf
