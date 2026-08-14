package lifecycle

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// gitIn runs a git command against dir using the per-repo test environment
// established by withTempRepo (isolated HOME and global config).
func gitIn(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = []string{
		"HOME=" + dir, "LANG=C", "PATH=" + os.Getenv("PATH"),
		"GIT_CONFIG_GLOBAL=" + filepath.Join(dir, "gitconfig"),
		"GIT_CONFIG_SYSTEM=/dev/null",
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v (%s)", args, err, out)
	}
}

// localConfigRead returns `git config --local --get key` for dir; missing
// values return ("", false).
func localConfigRead(t *testing.T, dir, key string) (string, bool) {
	t.Helper()
	cmd := exec.Command("git", "-C", dir, "config", "--local", "--get", key)
	cmd.Env = []string{
		"HOME=" + dir, "LANG=C", "PATH=" + os.Getenv("PATH"),
		"GIT_CONFIG_GLOBAL=" + filepath.Join(dir, "gitconfig"),
		"GIT_CONFIG_SYSTEM=/dev/null",
	}
	out, err := cmd.Output()
	if err != nil {
		return "", false
	}
	return strings.TrimSpace(string(out)), true
}

// writeExecHook writes an executable hook script into dir.
func writeExecHook(t *testing.T, dir, name, content string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func findInstalled(t *testing.T, m Manifest, kind HookKind) InstalledHook {
	t.Helper()
	for _, e := range m.Installed {
		if e.Kind == kind {
			return e
		}
	}
	t.Fatalf("manifest has no entry for %s: %+v", kind, m.Installed)
	return InstalledHook{}
}

// TestInstallPreservesPreExistingHooksPath covers the cold-review finding
// that install clobbered a pre-existing core.hooksPath and uninstall lost
// it forever. The prior value must be recorded and restored verbatim, and
// the foreign hook that lived there must keep running via a passthrough.
func TestInstallPreservesPreExistingHooksPath(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash unavailable")
	}
	dir := withTempRepo(t, true)
	custom := filepath.Join(dir, ".git", "custom-hooks")
	marker := filepath.Join(dir, "pre-commit-ran")
	customHook := writeExecHook(t, custom, "pre-commit",
		"#!/bin/sh\ntouch '"+marker+"'\n")
	gitIn(t, dir, "config", "--local", "core.hooksPath", ".git/custom-hooks")

	res, err := Install(Options{Root: dir, Strategy: StrategyRepoHooks})
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	if res.Manifest.PriorHooksPath != ".git/custom-hooks" || !res.Manifest.PriorHooksPathSet {
		t.Fatalf("prior hooksPath not recorded: %+v", res.Manifest)
	}
	if !samePath(res.Manifest.PriorHooksDir, custom) {
		t.Fatalf("prior hooks dir=%q want %q", res.Manifest.PriorHooksDir, custom)
	}

	// The foreign pre-commit hook must have a passthrough that keeps it live.
	shim := filepath.Join(dir, ".sentra", HooksDirName, "pre-commit")
	data, err := os.ReadFile(shim)
	if err != nil {
		t.Fatalf("passthrough shim missing: %v", err)
	}
	if !bytesHaveSentinel(data) || !strings.Contains(string(data), customHook) {
		t.Fatalf("shim does not delegate to prior hook:\n%s", data)
	}
	cmd := exec.Command(shim)
	cmd.Dir = dir
	cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=/nonexistent"}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("shim must keep the prior hook running: %v\n%s", err, out)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("prior hook did not run through the shim: %v", err)
	}
	live, _ := os.ReadFile(customHook)
	if string(live) != "#!/bin/sh\ntouch '"+marker+"'\n" {
		t.Fatalf("prior hook file was modified: %q", string(live))
	}

	// Uninstall must restore the prior hooksPath value verbatim and remove
	// every installed script.
	if _, err := Uninstall(Options{Root: dir, Strategy: StrategyRepoHooks}); err != nil {
		t.Fatalf("uninstall: %v", err)
	}
	got, ok := localConfigRead(t, dir, "core.hooksPath")
	if !ok || got != ".git/custom-hooks" {
		t.Fatalf("core.hooksPath=%q ok=%v want %q restored", got, ok, ".git/custom-hooks")
	}
	if _, err := os.Stat(shim); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("shim should be removed after uninstall: %v", err)
	}
	if _, err := os.Stat(customHook); err != nil {
		t.Fatalf("prior hook file disappeared: %v", err)
	}
}

// TestInstallDelegatesExistingManagedHook proves install never silently
// disables an existing hook of a managed kind: a real git commit must still
// run the repository's original post-commit hook through our script.
func TestInstallDelegatesExistingManagedHook(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash unavailable")
	}
	dir := withTempRepo(t, true)
	marker := filepath.Join(dir, "prior-post-commit-ran")
	prior := writeExecHook(t, filepath.Join(dir, ".git", "hooks"), "post-commit",
		"#!/bin/sh\ntouch '"+marker+"'\n")
	original, err := os.ReadFile(prior)
	if err != nil {
		t.Fatal(err)
	}

	res, err := Install(Options{Root: dir, Strategy: StrategyRepoHooks, Hooks: []HookKind{HookPostCommit}})
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	if !res.Manifest.PriorHooksPathSet {
		// No prior local hooksPath: uninstall must unset, not restore.
		if res.Manifest.PriorHooksPath != "" {
			t.Fatalf("unexpected prior hooksPath: %+v", res.Manifest)
		}
	}
	live, err := os.ReadFile(filepath.Join(dir, ".sentra", HooksDirName, "post-commit"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(live), prior) {
		t.Fatalf("installed script does not delegate to %s:\n%s", prior, live)
	}

	// End to end: git commit runs hooks via core.hooksPath; the original
	// post-commit must fire through our delegating script.
	gitIn(t, dir, "-c", "user.email=test@example.invalid", "-c", "user.name=test",
		"commit", "--allow-empty", "-m", "trigger hooks")
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("existing post-commit hook was disabled by install: %v", err)
	}
	after, _ := os.ReadFile(prior)
	if string(after) != string(original) {
		t.Fatalf("prior hook file was modified by install")
	}

	if _, err := Uninstall(Options{Root: dir, Strategy: StrategyRepoHooks}); err != nil {
		t.Fatal(err)
	}
	if got, ok := localConfigRead(t, dir, "core.hooksPath"); ok {
		t.Fatalf("core.hooksPath=%q should be unset after uninstall", got)
	}
}

// TestSequentialSubsetInstallsPreserveManifest covers the finding that a
// second subset install rewrote the manifest and orphaned the hooks from
// the first one. Every prior entry and snapshot must survive.
func TestSequentialSubsetInstallsPreserveManifest(t *testing.T) {
	dir := withTempRepo(t, true)
	first, err := Install(Options{Root: dir, Strategy: StrategyRepoHooks, Hooks: []HookKind{HookPostCommit}})
	if err != nil {
		t.Fatal(err)
	}
	firstEntry := findInstalled(t, first.Manifest, HookPostCommit)

	// A file appears at another managed slot before the second subset
	// install; its snapshot must be captured and restorable.
	hooksDir := filepath.Join(dir, ".sentra", HooksDirName)
	priorContent := "#!/bin/sh\n# user prior post-merge\nexit 0\n"
	writeExecHook(t, hooksDir, "post-merge", priorContent)

	second, err := Install(Options{Root: dir, Strategy: StrategyRepoHooks, Hooks: []HookKind{HookPostMerge}})
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Manifest.Installed) != 2 {
		t.Fatalf("second subset install dropped entries: %+v", second.Manifest.Installed)
	}
	carried := findInstalled(t, second.Manifest, HookPostCommit)
	if carried.ContentSHA != firstEntry.ContentSHA || carried.PriorSnapshot != firstEntry.PriorSnapshot {
		t.Fatalf("post-commit entry drifted across subset installs:\n first=%+v\n second=%+v",
			firstEntry, carried)
	}
	merged := findInstalled(t, second.Manifest, HookPostMerge)
	if merged.PriorSnapshot != priorContent {
		t.Fatalf("post-merge snapshot=%q want %q", merged.PriorSnapshot, priorContent)
	}

	status, err := Status(Options{Root: dir, Strategy: StrategyRepoHooks})
	if err != nil {
		t.Fatal(err)
	}
	if len(status.Installed) != 2 {
		t.Fatalf("status installed=%v want both subset hooks", status.Installed)
	}

	if _, err := Uninstall(Options{Root: dir, Strategy: StrategyRepoHooks}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(hooksDir, "post-commit")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("post-commit should be removed: %v", err)
	}
	rolled, err := os.ReadFile(filepath.Join(hooksDir, "post-merge"))
	if err != nil || string(rolled) != priorContent {
		t.Fatalf("post-merge not restored: %v %q", err, string(rolled))
	}
	if _, err := os.Stat(filepath.Join(dir, ".sentra", StateDirName, ManifestName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("manifest should be removed after uninstall: %v", err)
	}
}

// TestSequentialSubsetPreservesOriginalSnapshot ensures reinstalling a hook
// with different content (e.g. a new CLI path) never replaces the original
// pre-first-install snapshot with a snapshot of our own earlier script.
func TestSequentialSubsetPreservesOriginalSnapshot(t *testing.T) {
	dir := withTempRepo(t, true)
	hooksDir := filepath.Join(dir, ".sentra", HooksDirName)
	original := "#!/bin/sh\n# original user hook\nexit 0\n"
	path := writeExecHook(t, hooksDir, "post-commit", original)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	origMode := info.Mode().Perm()

	first, err := Install(Options{Root: dir, Strategy: StrategyRepoHooks,
		Hooks: []HookKind{HookPostCommit}, CLIExecutable: "/tmp/sentra-a"})
	if err != nil {
		t.Fatal(err)
	}
	firstEntry := findInstalled(t, first.Manifest, HookPostCommit)
	if firstEntry.PriorSnapshot != original {
		t.Fatalf("first snapshot=%q want original", firstEntry.PriorSnapshot)
	}

	second, err := Install(Options{Root: dir, Strategy: StrategyRepoHooks,
		Hooks: []HookKind{HookPostCommit}, CLIExecutable: "/tmp/sentra-b"})
	if err != nil {
		t.Fatal(err)
	}
	secondEntry := findInstalled(t, second.Manifest, HookPostCommit)
	if secondEntry.ContentSHA == firstEntry.ContentSHA {
		t.Fatalf("expected different script content for different CLI paths")
	}
	if secondEntry.PriorSnapshot != original {
		t.Fatalf("original snapshot lost on reinstall: %q", secondEntry.PriorSnapshot)
	}

	if _, err := Uninstall(Options{Root: dir, Strategy: StrategyRepoHooks}); err != nil {
		t.Fatal(err)
	}
	rolled, err := os.ReadFile(path)
	if err != nil || string(rolled) != original {
		t.Fatalf("rollback content=%q want %q", string(rolled), original)
	}
	info, err = os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != origMode {
		t.Fatalf("rollback mode=%v want %v", info.Mode().Perm(), origMode)
	}
}

// TestSecondInstallInheritsPriorHooksPath guards the re-install path: once
// our hooksPath is live, a second install must inherit the original prior
// value from the manifest instead of snapshotting our own value.
func TestSecondInstallInheritsPriorHooksPath(t *testing.T) {
	dir := withTempRepo(t, true)
	inactive := filepath.Join(dir, ".git", "other-hooks")
	if err := os.MkdirAll(inactive, 0o755); err != nil {
		t.Fatal(err)
	}
	gitIn(t, dir, "config", "--local", "core.hooksPath", ".git/other-hooks")

	first, err := Install(Options{Root: dir, Strategy: StrategyRepoHooks})
	if err != nil {
		t.Fatal(err)
	}
	if first.Manifest.PriorHooksPath != ".git/other-hooks" {
		t.Fatalf("first prior=%q want .git/other-hooks", first.Manifest.PriorHooksPath)
	}
	second, err := Install(Options{Root: dir, Strategy: StrategyRepoHooks})
	if err != nil {
		t.Fatal(err)
	}
	if second.Manifest.PriorHooksPath != ".git/other-hooks" || !second.Manifest.PriorHooksPathSet {
		t.Fatalf("second install lost the original prior hooksPath: %+v", second.Manifest)
	}
	if _, err := Uninstall(Options{Root: dir, Strategy: StrategyRepoHooks}); err != nil {
		t.Fatal(err)
	}
	got, ok := localConfigRead(t, dir, "core.hooksPath")
	if !ok || got != ".git/other-hooks" {
		t.Fatalf("core.hooksPath=%q ok=%v want .git/other-hooks", got, ok)
	}
}

// TestUninstallSkipsForeignReplacedHook ensures uninstall never destroys a
// hook file that is no longer sentra-managed.
func TestUninstallSkipsForeignReplacedHook(t *testing.T) {
	dir := withTempRepo(t, true)
	if _, err := Install(Options{Root: dir, Strategy: StrategyRepoHooks}); err != nil {
		t.Fatal(err)
	}
	hooksDir := filepath.Join(dir, ".sentra", HooksDirName)
	foreign := "#!/bin/sh\n# replaced by the user after install\nexit 0\n"
	path := filepath.Join(hooksDir, "post-commit")
	if err := os.WriteFile(path, []byte(foreign), 0o755); err != nil {
		t.Fatal(err)
	}

	res, err := Uninstall(Options{Root: dir, Strategy: StrategyRepoHooks})
	if err != nil {
		t.Fatal(err)
	}
	live, err := os.ReadFile(path)
	if err != nil || string(live) != foreign {
		t.Fatalf("foreign replacement file was destroyed: %v %q", err, string(live))
	}
	skipped := false
	for _, note := range res.Notes {
		if strings.Contains(note, "post-commit") && strings.Contains(note, "not sentra-managed") {
			skipped = true
		}
	}
	if !skipped {
		t.Fatalf("expected a skip note for post-commit: %+v", res.Notes)
	}
	// Every other installed hook is still removed.
	for _, k := range AllHooks {
		if k == HookPostCommit {
			continue
		}
		if _, err := os.Stat(filepath.Join(hooksDir, string(k))); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s should be removed: %v", k, err)
		}
	}
}

// TestInheritedHooksPathPreserved covers a prior hooksPath that comes from
// an inherited (non-local) config scope: install must still discover the
// active hooks, and uninstall must leave the inherited value effective by
// only unsetting the local override it created.
func TestInheritedHooksPathPreserved(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash unavailable")
	}
	dir := withTempRepo(t, true)
	globalHooks := t.TempDir()
	marker := filepath.Join(dir, "commit-msg-ran")
	writeExecHook(t, globalHooks, "commit-msg", "#!/bin/sh\ntouch '"+marker+"'\n")
	globalCfg := filepath.Join(t.TempDir(), "gitconfig")
	cfg := "[core]\n\thooksPath = " + globalHooks + "\n"
	if err := os.WriteFile(globalCfg, []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", globalCfg)

	res, err := Install(Options{Root: dir, Strategy: StrategyRepoHooks})
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	if res.Manifest.PriorHooksPathSet || res.Manifest.PriorHooksPath != "" {
		t.Fatalf("no local prior existed; manifest recorded %+v", res.Manifest)
	}
	if !samePath(res.Manifest.PriorHooksDir, globalHooks) {
		t.Fatalf("prior hooks dir=%q want inherited %q", res.Manifest.PriorHooksDir, globalHooks)
	}
	shim := filepath.Join(dir, ".sentra", HooksDirName, "commit-msg")
	cmd := exec.Command(shim)
	cmd.Dir = dir
	cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=/nonexistent"}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("commit-msg shim failed: %v\n%s", err, out)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("inherited commit-msg hook disabled by install: %v", err)
	}

	if _, err := Uninstall(Options{Root: dir, Strategy: StrategyRepoHooks}); err != nil {
		t.Fatal(err)
	}
	if got, ok := localConfigRead(t, dir, "core.hooksPath"); ok {
		t.Fatalf("local core.hooksPath=%q should be unset", got)
	}
	// The inherited value must remain effective after uninstall.
	cmd = exec.Command("git", "-C", dir, "config", "--get", "core.hooksPath")
	cmd.Env = []string{"HOME=" + dir, "LANG=C", "PATH=" + os.Getenv("PATH"),
		"GIT_CONFIG_GLOBAL=" + globalCfg, "GIT_CONFIG_SYSTEM=/dev/null"}
	out, err := cmd.Output()
	if err != nil || !samePath(strings.TrimSpace(string(out)), globalHooks) {
		t.Fatalf("effective hooksPath=%q err=%v want inherited %q", strings.TrimSpace(string(out)), err, globalHooks)
	}
}

// TestManifestRoundTripKeepsPriorFields guards the manifest JSON shape: the
// prior-hooksPath fields survive a marshal/unmarshal round trip.
func TestManifestRoundTripKeepsPriorFields(t *testing.T) {
	m := Manifest{
		Schema:            ManifestSchema,
		PriorHooksPath:    ".git/custom-hooks",
		PriorHooksPathSet: true,
		PriorHooksDir:     "/repo/.git/custom-hooks",
	}
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	var back Manifest
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	if back.PriorHooksPath != m.PriorHooksPath || !back.PriorHooksPathSet || back.PriorHooksDir != m.PriorHooksDir {
		t.Fatalf("round trip lost prior fields: %+v", back)
	}
}

// loadManifestForTest reads the on-disk manifest for assertions about what
// an interrupted or failed install persisted.
func loadManifestForTest(t *testing.T, stateDir string) Manifest {
	t.Helper()
	m, exists, err := loadManifest(stateDir)
	if err != nil {
		t.Fatalf("load manifest: %v", err)
	}
	if !exists {
		t.Fatalf("manifest missing at %s", stateDir)
	}
	return *m
}

// TestPartialGitCommonInstallFailureIsRecoverable forces a mid-install
// failure: the shared git-common hooks directory already contains a foreign
// (non-sentra) post-merge hook, which installScript refuses to overwrite.
// Sorted install order means post-checkout and post-commit are written
// before the refusal. The checkpointed manifest must record exactly those
// mutations so Uninstall rolls them back while preserving the foreign hook.
func TestPartialGitCommonInstallFailureIsRecoverable(t *testing.T) {
	dir := withTempRepo(t, true)
	gitCommon := filepath.Join(dir, ".git")
	sharedHooks := filepath.Join(gitCommon, HooksDirName)
	foreign := writeExecHook(t, sharedHooks, "post-merge", "#!/bin/sh\necho foreign\n")

	_, err := Install(Options{Root: dir, Strategy: StrategyGitCommon, AllowUnsafeGitCommon: true})
	if err == nil {
		t.Fatal("install must fail on the foreign post-merge hook")
	}
	if !strings.Contains(err.Error(), "post-merge") {
		t.Fatalf("error should name the refusing hook, got %v", err)
	}

	// The partial install must have left a manifest covering what it wrote.
	stateDir := filepath.Join(gitCommon, StateDirName)
	m := loadManifestForTest(t, stateDir)
	if len(m.Installed) != 2 {
		t.Fatalf("manifest should record exactly the two written hooks, got %+v", m.Installed)
	}
	for _, e := range m.Installed {
		if e.PriorExisted {
			t.Fatalf("%s had no prior file; snapshot must say so: %+v", e.Kind, e)
		}
	}

	// Uninstall restores the pre-install state: the two sentra hooks are
	// removed, the foreign hook is preserved, and the manifest is gone.
	res, err := Uninstall(Options{Root: dir, Strategy: StrategyGitCommon, AllowUnsafeGitCommon: true})
	if err != nil {
		t.Fatalf("uninstall of partial install: %v", err)
	}
	if !res.OK {
		t.Fatalf("uninstall not ok: %+v", res)
	}
	for _, name := range []string{"post-checkout", "post-commit"} {
		if _, statErr := os.Stat(filepath.Join(sharedHooks, name)); !os.IsNotExist(statErr) {
			t.Fatalf("%s should be removed by uninstall, stat err=%v", name, statErr)
		}
	}
	data, readErr := os.ReadFile(foreign)
	if readErr != nil || !strings.Contains(string(data), "foreign") {
		t.Fatalf("foreign post-merge hook must survive uninstall: %v %q", readErr, data)
	}
	if _, statErr := os.Stat(filepath.Join(stateDir, ManifestName)); !os.IsNotExist(statErr) {
		t.Fatalf("manifest should be removed after uninstall, stat err=%v", statErr)
	}
}

// TestInstallCheckpointsBeforeHooksPathFlip forces setLocalHooksPath to
// fail (read-only .git directory) after the pre-flip checkpoint. The
// checkpointed manifest must already record the pending HooksPath and the
// prior-state fields, the live git config must be untouched, and Uninstall
// must cleanly unwind to the pre-install state.
func TestInstallCheckpointsBeforeHooksPathFlip(t *testing.T) {
	dir := withTempRepo(t, true)
	gitDir := filepath.Join(dir, ".git")
	if err := os.Chmod(gitDir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(gitDir, 0o755) })

	_, err := Install(Options{Root: dir, Strategy: StrategyRepoHooks})
	if err == nil {
		t.Fatal("install must fail when git cannot write .git/config")
	}

	stateDir := filepath.Join(dir, ".sentra", StateDirName)
	m := loadManifestForTest(t, stateDir)
	// Install resolves symlinks on root (ResolveRoot), so compare against
	// the resolved hooks dir — /var vs /private/var on macOS.
	realDir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	hooksDir := filepath.Join(realDir, ".sentra", HooksDirName)
	if m.HooksPath != hooksDir {
		t.Fatalf("checkpoint must record the pending hooksPath %q, got %q", hooksDir, m.HooksPath)
	}
	if m.PriorHooksPathSet {
		t.Fatalf("no prior local hooksPath existed; checkpoint claims otherwise: %+v", m)
	}
	// The config flip never happened, so the live value must still be unset.
	if got, ok := localConfigRead(t, dir, "core.hooksPath"); ok {
		t.Fatalf("live core.hooksPath=%q should be unset after failed flip", got)
	}

	if _, err := Uninstall(Options{Root: dir, Strategy: StrategyRepoHooks}); err != nil {
		t.Fatalf("uninstall after failed flip: %v", err)
	}
	if got, ok := localConfigRead(t, dir, "core.hooksPath"); ok {
		t.Fatalf("core.hooksPath=%q should remain unset after uninstall", got)
	}
	if _, statErr := os.Stat(filepath.Join(stateDir, ManifestName)); !os.IsNotExist(statErr) {
		t.Fatalf("manifest should be removed after uninstall, stat err=%v", statErr)
	}
}

// TestInstallScriptPreWriteCheckpointContract locks the ordering guarantee
// behind crash safety: preWrite runs AFTER the prior-state snapshot is
// captured but BEFORE the file is replaced, and a preWrite failure aborts
// the write so no mutation ever happens without a persisted rollback
// record.
func TestInstallScriptPreWriteCheckpointContract(t *testing.T) {
	hooksDir := t.TempDir()
	target := filepath.Join(hooksDir, "post-commit")
	if err := os.WriteFile(target, []byte("original"), 0o755); err != nil {
		t.Fatal(err)
	}

	called := false
	entry, wrote, err := installScript(hooksDir, "post-commit", []byte("replacement"),
		InstalledHook{}, false, StrategyRepoHooks, func(e InstalledHook) error {
			called = true
			// At checkpoint time the prior file must still be on disk and the
			// entry must already carry its bytes for rollback.
			data, readErr := os.ReadFile(target)
			if readErr != nil || string(data) != "original" {
				t.Fatalf("preWrite ran after the write (or read failed): %v %q", readErr, data)
			}
			if !e.PriorExisted || e.PriorSnapshot != "original" {
				t.Fatalf("checkpoint entry missing prior snapshot: %+v", e)
			}
			return nil
		})
	if err != nil || !wrote {
		t.Fatalf("installScript: wrote=%v err=%v", wrote, err)
	}
	if !called {
		t.Fatal("preWrite was not invoked before the write")
	}
	if data, _ := os.ReadFile(target); string(data) != "replacement" {
		t.Fatalf("target not replaced: %q", data)
	}
	if entry.PriorSnapshot != "original" {
		t.Fatalf("entry lost prior snapshot: %+v", entry)
	}

	// A failing preWrite fails the install closed: no write happens.
	boom := errors.New("checkpoint unavailable")
	if _, wrote, err := installScript(hooksDir, "pre-push", []byte("new"),
		InstalledHook{}, false, StrategyRepoHooks, func(InstalledHook) error { return boom }); !errors.Is(err, boom) || wrote {
		t.Fatalf("preWrite failure must abort the write: wrote=%v err=%v", wrote, err)
	}
	if _, statErr := os.Stat(filepath.Join(hooksDir, "pre-push")); !os.IsNotExist(statErr) {
		t.Fatalf("pre-push must not exist after aborted write, stat err=%v", statErr)
	}
}

// TestReinstallPreservesPriorInstallStateEndToEnd is the F2 regression
// test for the user-facing invariant: a successful install, a successful
// reinstall (with different content so every hook is rewritten), and an
// uninstall must restore the repository to its pre-first-install state
// — the original core.hooksPath, the original hook files, and the
// absence of any .sentra/ directory.
//
// The checkpoint changes that protected partial failures must not have
// regressed this round-trip. The intermediate manifest must carry the
// original pre-install state across every checkpoint, and the final
// uninstall must read it back.
func TestReinstallPreservesPriorInstallStateEndToEnd(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash unavailable")
	}
	dir := withTempRepo(t, true)
	custom := filepath.Join(dir, ".git", "custom-hooks")
	marker := filepath.Join(dir, "post-commit-ran")
	customHook := writeExecHook(t, custom, "post-commit",
		"#!/bin/sh\ntouch '"+marker+"'\n")
	gitIn(t, dir, "config", "--local", "core.hooksPath", ".git/custom-hooks")

	first, err := Install(Options{Root: dir, Strategy: StrategyRepoHooks, CLIExecutable: "/tmp/sentra-a"})
	if err != nil {
		t.Fatalf("first install: %v", err)
	}
	if first.Manifest.PriorHooksPath != ".git/custom-hooks" || !first.Manifest.PriorHooksPathSet {
		t.Fatalf("first install did not record pre-install hooksPath: %+v", first.Manifest)
	}

	// Reinstall with a different CLI executable so every hook script is
	// rewritten — this is the path the checkpoint changes are most likely
	// to perturb because preWrite fires for every hook.
	second, err := Install(Options{Root: dir, Strategy: StrategyRepoHooks, CLIExecutable: "/tmp/sentra-b"})
	if err != nil {
		t.Fatalf("reinstall: %v", err)
	}
	if second.Manifest.PriorHooksPath != ".git/custom-hooks" || !second.Manifest.PriorHooksPathSet {
		t.Fatalf("reinstall lost the pre-install hooksPath: %+v", second.Manifest)
	}
	if !samePath(second.Manifest.PriorHooksDir, custom) {
		t.Fatalf("reinstall prior hooks dir=%q want %q", second.Manifest.PriorHooksDir, custom)
	}

	// Uninstall must restore the original hooksPath value verbatim.
	if _, err := Uninstall(Options{Root: dir, Strategy: StrategyRepoHooks}); err != nil {
		t.Fatalf("uninstall: %v", err)
	}
	got, ok := localConfigRead(t, dir, "core.hooksPath")
	if !ok || got != ".git/custom-hooks" {
		t.Fatalf("core.hooksPath=%q ok=%v want %q restored after uninstall", got, ok, ".git/custom-hooks")
	}
	// Every installed hook must be removed.
	hooksDir := filepath.Join(dir, ".sentra", HooksDirName)
	for _, k := range AllHooks {
		if _, err := os.Stat(filepath.Join(hooksDir, string(k))); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("hook %s should be removed after uninstall: %v", k, err)
		}
	}
	// The pre-install custom hook must still be byte-identical.
	live, err := os.ReadFile(customHook)
	if err != nil {
		t.Fatalf("pre-install hook disappeared: %v", err)
	}
	if string(live) != "#!/bin/sh\ntouch '"+marker+"'\n" {
		t.Fatalf("pre-install hook content changed: %q", string(live))
	}
	// The manifest file must be removed.
	if _, err := os.Stat(filepath.Join(dir, ".sentra", StateDirName, ManifestName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("manifest should be removed after uninstall: %v", err)
	}
}

// TestReinstallCheckpointIncludesCarriedForwardEntries locks the core
// invariant that the F2 regression fix protects: every checkpoint the
// installer writes during a reinstall must contain the carried-forward
// prev entries — not just the entries the loop appended. We exercise
// this by doing a successful first install (4 hooks), then a reinstall
// that fails partway because installScript refuses to overwrite a
// foreign hook in the shared hooks directory. Without the carry-forward
// in checkpoint, the manifest on disk after the failure would only
// describe the hooks the loop successfully wrote; the carried-forward
// prev entries would be lost and Uninstall would leave those hooks in
// whatever state the failed install left them. With the fix, every
// checkpoint includes the carried-forward entries so Uninstall can
// restore every prior state even from a partial-failure manifest.
func TestReinstallCheckpointIncludesCarriedForwardEntries(t *testing.T) {
	dir := withTempRepo(t, true)
	gitCommon := filepath.Join(dir, ".git")
	sharedHooks := filepath.Join(gitCommon, HooksDirName)

	// First install: all 4 hooks under the shared git-common dir.
	first, err := Install(Options{Root: dir, Strategy: StrategyGitCommon,
		AllowUnsafeGitCommon: true, CLIExecutable: "/tmp/sentra-a"})
	if err != nil {
		t.Fatalf("first install: %v", err)
	}
	if len(first.Manifest.Installed) != len(AllHooks) {
		t.Fatalf("first install wrote %d hooks, want %d: %+v",
			len(first.Manifest.Installed), len(AllHooks), first.Manifest.Installed)
	}

	// Replace the sentra-managed post-merge hook with a foreign hook
	// so the second install fails partway when installScript refuses to
	// overwrite a non-sentra hook in the shared hooks dir.
	foreign := "#!/bin/sh\n# foreign tool's post-merge hook\necho foreign\n"
	if err := os.WriteFile(filepath.Join(sharedHooks, "post-merge"), []byte(foreign), 0o755); err != nil {
		t.Fatal(err)
	}

	// Second install with a DIFFERENT CLI executable so every hook
	// script is rewritten and preWrite fires for each. The sorted
	// install order is post-commit, post-checkout, post-merge, pre-push
	// (AllHooks). The first two succeed; post-merge refuses; pre-push
	// is never attempted. The on-disk manifest must still describe
	// every prev entry so a subsequent Uninstall can restore them.
	_, err = Install(Options{Root: dir, Strategy: StrategyGitCommon,
		AllowUnsafeGitCommon: true, CLIExecutable: "/tmp/sentra-b"})
	if err == nil {
		t.Fatal("reinstall must fail on the foreign post-merge hook")
	}
	if !strings.Contains(err.Error(), "post-merge") {
		t.Fatalf("error should name the refusing hook, got %v", err)
	}

	// Read the on-disk manifest that the failing install left behind.
	// Without the F2 fix, this manifest would have only post-commit and
	// post-checkout (the hooks the loop successfully wrote). With the
	// fix, the checkpoint before each write included the carried-forward
	// prev entries, so the manifest also describes post-merge and
	// pre-push and Uninstall can restore them.
	stateDir := filepath.Join(gitCommon, StateDirName)
	onDisk := loadManifestForTest(t, stateDir)
	if len(onDisk.Installed) != len(AllHooks) {
		t.Fatalf("post-failure manifest lost carried-forward entries: have %d, want %d (%+v)",
			len(onDisk.Installed), len(AllHooks), onDisk.Installed)
	}
	for _, e := range first.Manifest.Installed {
		found := false
		for _, got := range onDisk.Installed {
			if got.Kind == e.Kind {
				found = true
				if got.Kind == "post-merge" || got.Kind == "pre-push" {
					// Carried-forward entries preserve the original
					// prior snapshot so Uninstall can restore the
					// pre-first-install state.
					if got.PriorSnapshot != e.PriorSnapshot {
						t.Fatalf("carried-forward entry %s lost prior snapshot:\n first=%q\n onDisk=%q",
							got.Kind, e.PriorSnapshot, got.PriorSnapshot)
					}
				}
				break
			}
		}
		if !found {
			t.Fatalf("manifest on disk missing entry for %s after partial-failure reinstall: %+v",
				e.Kind, onDisk.Installed)
		}
	}

	// Uninstall must restore every prior state from the manifest,
	// including the carried-forward entries — the foreign post-merge
	// hook is preserved (installScript refused to touch it) and the
	// manifest is removed so a fresh install starts clean.
	if _, err := Uninstall(Options{Root: dir, Strategy: StrategyGitCommon,
		AllowUnsafeGitCommon: true}); err != nil {
		t.Fatalf("uninstall after partial-failure reinstall: %v", err)
	}
	// Foreign hook survives.
	if live, err := os.ReadFile(filepath.Join(sharedHooks, "post-merge")); err != nil ||
		!strings.Contains(string(live), "foreign") {
		t.Fatalf("foreign post-merge hook must survive uninstall: %v %q", err, string(live))
	}
	// Sentra hooks the loop wrote are removed; the carried-forward
	// entries that were never written never had a file to remove.
	for _, name := range []string{"post-commit", "post-checkout", "pre-push"} {
		if _, statErr := os.Stat(filepath.Join(sharedHooks, name)); !os.IsNotExist(statErr) {
			t.Fatalf("%s should be removed after uninstall, stat err=%v", name, statErr)
		}
	}
	// Manifest file is removed.
	if _, statErr := os.Stat(filepath.Join(stateDir, ManifestName)); !os.IsNotExist(statErr) {
		t.Fatalf("manifest should be removed after uninstall, stat err=%v", statErr)
	}
}

// TestReinstallNoOpByteIdenticalManifest is the no-op reinstall lock:
// reinstalling with identical content (same CLI executable, same
// hooks set, same strategy) must produce a byte-identical manifest so
// repeated CI runs do not drift mtimes or timestamps. The
// carryForward inside checkpoint must not perturb the no-op signature.
func TestReinstallNoOpByteIdenticalManifest(t *testing.T) {
	dir := withTempRepo(t, true)
	first, err := Install(Options{Root: dir, Strategy: StrategyRepoHooks, CLIExecutable: "/tmp/sentra-c"})
	if err != nil {
		t.Fatalf("first install: %v", err)
	}
	second, err := Install(Options{Root: dir, Strategy: StrategyRepoHooks, CLIExecutable: "/tmp/sentra-c"})
	if err != nil {
		t.Fatalf("second install: %v", err)
	}
	if first.Manifest.InstalledAt != second.Manifest.InstalledAt {
		t.Fatalf("no-op reinstall must preserve InstalledAt timestamp: first=%s second=%s",
			first.Manifest.InstalledAt, second.Manifest.InstalledAt)
	}
	// Compare the canonical manifest JSON. Notes intentionally differ
	// between the first and second install (the first flips
	// core.hooksPath and emits a "set git config ..." note; the second
	// is a true no-op and emits no such note); the manifest itself is
	// what the no-op-detection signature lock protects.
	firstRaw, _ := json.Marshal(first.Manifest)
	secondRaw, _ := json.Marshal(second.Manifest)
	if string(firstRaw) != string(secondRaw) {
		t.Fatalf("no-op reinstall produced manifest drift:\n first=%s\n second=%s", firstRaw, secondRaw)
	}
}

// TestReinstallSubsetPreservesPriorState locks the subset-reinstall
// invariant: when a subsequent install targets a strict subset of
// kinds, the carried-forward entries for kinds the run does not touch
// must still carry the original pre-install snapshot. The checkpoint
// for the kind that IS rewritten must include the carried-forward
// entries so uninstall can restore every prior state, not just the
// touched kinds.
func TestReinstallSubsetPreservesPriorState(t *testing.T) {
	dir := withTempRepo(t, true)
	inactive := filepath.Join(dir, ".git", "other-hooks")
	if err := os.MkdirAll(inactive, 0o755); err != nil {
		t.Fatal(err)
	}
	gitIn(t, dir, "config", "--local", "core.hooksPath", ".git/other-hooks")

	first, err := Install(Options{Root: dir, Strategy: StrategyRepoHooks, CLIExecutable: "/tmp/sentra-d"})
	if err != nil {
		t.Fatalf("first install: %v", err)
	}
	if first.Manifest.PriorHooksPath != ".git/other-hooks" || !first.Manifest.PriorHooksPathSet {
		t.Fatalf("first install lost pre-install hooksPath: %+v", first.Manifest)
	}

	// Subset reinstall that rewrites only one hook. The carried-forward
	// entries for the OTHER three hooks must survive in the manifest on
	// disk after the checkpoint for the rewritten hook fires. We verify
	// by reading the manifest immediately after the reinstall completes.
	second, err := Install(Options{Root: dir, Strategy: StrategyRepoHooks,
		Hooks: []HookKind{HookPostCommit}, CLIExecutable: "/tmp/sentra-e"})
	if err != nil {
		t.Fatalf("subset reinstall: %v", err)
	}
	if second.Manifest.PriorHooksPath != ".git/other-hooks" || !second.Manifest.PriorHooksPathSet {
		t.Fatalf("subset reinstall lost pre-install hooksPath: %+v", second.Manifest)
	}
	if len(second.Manifest.Installed) != len(AllHooks) {
		t.Fatalf("subset reinstall manifest has %d entries, want %d: %+v",
			len(second.Manifest.Installed), len(AllHooks), second.Manifest.Installed)
	}

	// Uninstall restores the original state.
	if _, err := Uninstall(Options{Root: dir, Strategy: StrategyRepoHooks}); err != nil {
		t.Fatalf("uninstall: %v", err)
	}
	got, ok := localConfigRead(t, dir, "core.hooksPath")
	if !ok || got != ".git/other-hooks" {
		t.Fatalf("core.hooksPath=%q ok=%v want %q", got, ok, ".git/other-hooks")
	}
}
