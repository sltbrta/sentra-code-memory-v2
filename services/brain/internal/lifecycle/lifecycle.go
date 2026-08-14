// Package lifecycle owns the local-only git hook installer: install, status,
// and uninstall for hook scripts confined to <root>/.sentra/hooks/.
//
// The goal is a tiny, tightly-scoped control plane for the standalone
// local-first CLI: hook scripts are written only inside the per-repo
// .sentra/ directory (or, on explicit opt-in, the shared git common
// directory's hooks/), they are atomic, idempotent, and a complete rollback
// snapshot is preserved in a JSON manifest so uninstall restores prior state
// bit-perfect.
//
// The repo-hooks strategy additionally records the pre-existing
// core.hooksPath value in the manifest and never silently disables a hook
// that git used to run: hooks for kinds this installer manages delegate to
// the prior hook script, and every other active hook in the prior hooks
// directory gets a passthrough script. Uninstall restores the prior
// core.hooksPath value (or unsets it when there was none).
//
// This package is intentionally offline. It does not import net/http or any
// networking package. All writes are local files. The git binary is invoked
// with -c core.hooksPath=/dev/null so the installer cannot recurse into
// itself, and credential helpers are scrubbed from the environment for the
// same reason.
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
	"sort"
	"strings"
	"sync"
	"time"
)

// HookKind is the bounded set of hook names this installer knows about.
// Anything outside the set is rejected up front (preventing path-traversal
// or arbitrary execution).
type HookKind string

const (
	HookPostCommit   HookKind = "post-commit"
	HookPostCheckout HookKind = "post-checkout"
	HookPostMerge    HookKind = "post-merge"
	HookPrePush      HookKind = "pre-push"
)

// AllHooks is the canonical ordered list of supported hooks. Callers expose
// this slice; we never iterate an internal unsorted map.
var AllHooks = []HookKind{
	HookPostCommit,
	HookPostCheckout,
	HookPostMerge,
	HookPrePush,
}

// SentinelHeader is the first line of every installed hook script. The
// install process looks for this exact byte sequence so an unrelated hook
// belonging to another tool is never overwritten or removed.
const SentinelHeader = "# sentra-lifecycle:v1\n"

// Sentinels directory layout: <root>/.sentra/hooks/<kind>  +  <root>/.sentra/hooks/sentra-manifest.json
const (
	HooksDirName  = "hooks"
	ManifestName  = "sentra-manifest.json"
	StateDirName  = "state"
	SnapshotName  = "sentra-snapshot.json"
	hooksOwnerKey = "sentra-lifecycle"
)

// Strategy controls where hook scripts land. RepoHooks is the only safe
// default; GitCommonHooks requires explicit opt-in and refuses if the git
// common directory is not writable to the caller.
type Strategy string

const (
	// StrategyRepoHooks writes to <root>/.sentra/hooks and, when root is a
	// git repository, sets core.hooksPath there so the hook scripts take
	// effect for subsequent git operations on this checkout. The prior
	// core.hooksPath value (local scope) is recorded in the manifest and
	// restored on uninstall; hooks already active in the prior hooks
	// directory keep working through delegation or passthrough scripts.
	// Fully reversed by uninstall.
	StrategyRepoHooks Strategy = "repo-hooks"
	// StrategyGitCommon writes to <git-common-dir>/hooks (the shared
	// repository hooks directory). Per-checkout core.hooksPath is left
	// untouched. Only allowed when root is a git repository.
	StrategyGitCommon Strategy = "git-common-hooks"
)

// Options carries the inputs for every lifecycle verb. Zero-value fields use
// documented defaults so callers can pass Options{} for the common path.
type Options struct {
	// Root is the repository root. When empty, the current working
	// directory is used. Must exist and be an absolute or "." path.
	Root string
	// Strategy selects the destination layout. The default is
	// StrategyRepoHooks, the only one safe to invoke without explicit
	// reviewer approval because hook files stay under <root>/.sentra/ and
	// the only git mutation is the local core.hooksPath setting.
	Strategy Strategy
	// Hooks restricts the install set. When empty, AllHooks is used.
	// Uninstall always restores every hook recorded in the manifest, and
	// Status reports every installed hook regardless of Hooks.
	Hooks []HookKind
	// AllowUnsafeGitCommon must be true to opt into StrategyGitCommon. The
	// non-zero requirement is defensive even though Strategy==GitCommon is
	// the trigger — it forces callers to spell out the consent.
	AllowUnsafeGitCommon bool
	// CLIExecutable is the sentra-code-memory binary path invoked by
	// installed hook scripts. When empty, $PATH lookup is performed at hook
	// run-time, which means the hook will exit cleanly (status 0) without
	// forwarding the event if the binary is not installed.
	CLIExecutable string
	// Quiet suppresses human-readable progress; responses still report state.
	Quiet bool
}

// Manifest records the installed hooks and a snapshot of every prior state
// for true rollback. Schema versioning lets future versions reject unknown
// formats on load rather than silently corrupting state.
type Manifest struct {
	Schema    string          `json:"schema"`
	Root      string          `json:"root"`
	Strategy  Strategy        `json:"strategy"`
	HooksDir  string          `json:"hooks_dir"`
	Installed []InstalledHook `json:"installed"`
	// HooksPath is the git value of core.hooksPath after install. Empty
	// when Strategy != StrategyRepoHooks. Uninstall restores the prior
	// value recorded below (or clears the setting when there was none).
	HooksPath string `json:"hooks_path,omitempty"`
	// PriorHooksPath is the local core.hooksPath value recorded before the
	// first install flipped it. Empty when no local value existed. Uninstall
	// writes it back when the live value still matches HooksPath.
	PriorHooksPath string `json:"prior_hooks_path,omitempty"`
	// PriorHooksPathSet distinguishes "no prior local value" from an empty
	// string stored in git config.
	PriorHooksPathSet bool `json:"prior_hooks_path_set,omitempty"`
	// PriorHooksDir is the directory git read hooks from before the install
	// (the resolved prior hooksPath, or <git-common-dir>/hooks by default).
	// Sequential installs reuse it so re-installs never rescan our own
	// hooks directory.
	PriorHooksDir string `json:"prior_hooks_dir,omitempty"`
	InstalledAt   string `json:"installed_at,omitempty"`
	CLIExecutable string `json:"cli_executable,omitempty"`
}

// InstalledHook is one entry in the manifest with both the live script
// content (for verification) and the prior byte snapshot (for rollback).
type InstalledHook struct {
	Kind         HookKind `json:"kind"`
	Path         string   `json:"path"`
	ContentSHA   string   `json:"content_sha256"`
	Mode         string   `json:"mode"`
	PriorPath    string   `json:"prior_path,omitempty"`
	PriorExisted bool     `json:"prior_existed"`
	// PriorSnapshot is the verbatim file contents (base64-safe text since
	// hook scripts are bash) before this installer touched the path. Empty
	// when PriorExisted is false.
	PriorSnapshot string `json:"prior_snapshot,omitempty"`
	// PriorMode is the file mode octal (e.g. "0755") of the prior file,
	// preserved so uninstall restores the original permissions.
	PriorMode string `json:"prior_mode,omitempty"`
}

// Status is the read-only view of one local install. Stable JSON shape.
type Report struct {
	OK         bool     `json:"ok"`
	Schema     string   `json:"schema"`
	Root       string   `json:"root"`
	Strategy   Strategy `json:"strategy,omitempty"`
	HooksDir   string   `json:"hooks_dir,omitempty"`
	HooksPath  string   `json:"hooks_path,omitempty"`
	Installed  []string `json:"installed,omitempty"`
	Missing    []string `json:"missing,omitempty"`
	Unexpected []string `json:"unexpected,omitempty"`
	Manifest   Manifest `json:"manifest,omitempty"`
	Notes      []string `json:"notes,omitempty"`
}

// Result is the install/uninstall response envelope.
type Result struct {
	OK       bool     `json:"ok"`
	Action   string   `json:"action"`
	Strategy Strategy `json:"strategy,omitempty"`
	Manifest Manifest `json:"manifest"`
	Notes    []string `json:"notes,omitempty"`
}

// ManifestSchema is the on-disk format version. Bump on any incompatible
// change so old binaries fail closed rather than corrupting state.
const ManifestSchema = "sentra.lifecycle.manifest/v1"

// Errors returned by this package. All of them are validation failures that
// fail closed (no filesystem state is touched on error).
var (
	ErrNotGitRepo       = errors.New("lifecycle: root is not a git repository")
	ErrHooksPath        = errors.New("lifecycle: hooksPath cannot be set")
	ErrPathNotAllowed   = errors.New("lifecycle: target path escapes allowed directories")
	ErrUnknownHookKind  = errors.New("lifecycle: unknown hook kind")
	ErrUnsafeGitCommon  = errors.New("lifecycle: StrategyGitCommon requires AllowUnsafeGitCommon=true and a git repository")
	ErrRootMissing      = errors.New("lifecycle: root does not exist or is not a directory")
	ErrManifestCorrupt  = errors.New("lifecycle: manifest exists but is corrupt")
	ErrInstalledByOther = errors.New("lifecycle: hook file at target path is owned by another tool")
)

// gitMu serializes installs that need to flip core.hooksPath. Concurrent
// installs against different repos can still proceed in parallel because the
// mutex is keyed by root.
var gitMu sync.Map // string (canonical root) → *sync.Mutex

func guardFor(root string) *sync.Mutex {
	if v, ok := gitMu.Load(root); ok {
		return v.(*sync.Mutex)
	}
	mu := &sync.Mutex{}
	actual, _ := gitMu.LoadOrStore(root, mu)
	return actual.(*sync.Mutex)
}

// ResolveRoot normalizes opts.Root and returns an absolute, symlink-resolved
// directory path. "." resolves to the current working directory.
func ResolveRoot(opts Options) (string, error) {
	root := strings.TrimSpace(opts.Root)
	if root == "" {
		root = "."
	}
	if !filepath.IsAbs(root) {
		abs, err := filepath.Abs(root)
		if err != nil {
			return "", err
		}
		root = abs
	}
	info, err := os.Stat(root)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrRootMissing, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("%w: %q is not a directory", ErrRootMissing, root)
	}
	if real, err := filepath.EvalSymlinks(root); err == nil {
		root = real
	}
	return root, nil
}

// IsGitRepo returns true when root has a .git directory or is a worktree
// under a shared git directory. This is a structural check; callers that
// need to drive `git` commands must additionally use a hooksPath=/dev/null
// prefix so the installer cannot recurse.
func IsGitRepo(root string) bool {
	if filepath.Base(root) == ".git" {
		return true
	}
	gi, err := os.Stat(filepath.Join(root, ".git"))
	if err == nil {
		return gi.IsDir() || (gi.Mode()&os.ModeSymlink != 0)
	}
	// Worktree: a .git *file* containing "gitdir: ..." is also valid.
	if data, err := os.ReadFile(filepath.Join(root, ".git")); err == nil {
		return strings.HasPrefix(string(data), "gitdir:")
	}
	return false
}

// gitCommonDir returns the shared git directory for root. When root is not a
// git repository the error from git is propagated as ErrNotGitRepo. The git
// invocation is sandboxed so it cannot recurse into any installed hooks.
//
// rev-parse returns the path "as recorded" by default (often ".git" for a
// non-worktree checkout), which is not useful for downstream path joining;
// --path-format=absolute normalizes the result against the cwd.
func gitCommonDir(root string) (string, error) {
	if !IsGitRepo(root) {
		return "", ErrNotGitRepo
	}
	cmd := exec.Command("git", "-C", root, "-c", "core.hooksPath=/dev/null",
		"-c", "core.fsmonitor=false", "-c", "credential.helper=", "rev-parse",
		"--path-format=absolute", "--git-common-dir")
	cmd.Env = []string{"HOME=/nonexistent", "LANG=C", "PATH=" + os.Getenv("PATH")}
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrNotGitRepo, err)
	}
	dir := strings.TrimSpace(string(out))
	if dir == "" {
		return "", ErrNotGitRepo
	}
	if !filepath.IsAbs(dir) {
		dir = filepath.Join(root, dir)
	}
	if real, err := filepath.EvalSymlinks(dir); err == nil {
		dir = real
	}
	return dir, nil
}

// resolveHooksDir returns the absolute hooks directory path for the chosen
// strategy. The returned dir is allowed to be the parent of both the new
// script and the manifest; callers MUST verify that dir is contained under
// root (StrategyRepoHooks) or under gitCommonDir (StrategyGitCommon) before
// invoking writeHook.
func resolveHooksDir(root string, strategy Strategy, gitCommon string) (string, error) {
	switch strategy {
	case StrategyRepoHooks:
		return filepath.Join(root, ".sentra", HooksDirName), nil
	case StrategyGitCommon:
		if gitCommon == "" {
			return "", ErrUnsafeGitCommon
		}
		return filepath.Join(gitCommon, HooksDirName), nil
	default:
		return "", fmt.Errorf("lifecycle: unknown strategy %q", strategy)
	}
}

// ensureConfined verifies that target is contained under allowedRoot using a
// lexical (path string) check rather than per-component traversal, so a
// symlink race cannot escape. We Resolve path components and require every
// segment to descend from allowedRoot's resolved path.
func ensureConfined(target, allowedRoot string) error {
	realTarget, terr := filepath.EvalSymlinks(filepath.Dir(target))
	if terr != nil {
		realTarget = filepath.Dir(target)
	}
	realAllowed, aerr := filepath.EvalSymlinks(allowedRoot)
	if aerr != nil {
		realAllowed = allowedRoot
	}
	rel, err := filepath.Rel(realAllowed, filepath.Join(realTarget, filepath.Base(target)))
	if err != nil || strings.HasPrefix(rel, "..") || strings.Contains(rel, "..") {
		return ErrPathNotAllowed
	}
	return nil
}

// writeHookFile atomically writes content to path with mode. The dance is the
// standard temp-fsync-rename-parent-fsync sequence; an interrupted write
// leaves the prior file intact. We never overwrite non-sentra hooks.
func writeHookFile(path string, content []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".sentra-hook-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if _, err := tmp.Write(content); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	d, err := os.Open(dir)
	if err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}

// readFileIfExists is a helper that returns (nil, false, nil) for missing
// files so callers don't have to branch on os.IsNotExist everywhere.
func readFileIfExists(path string) ([]byte, bool, error) {
	data, err := os.ReadFile(path)
	if err == nil {
		return data, true, nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	return nil, false, err
}

// renderScript returns the bash hook body. The script:
//   - exits 0 if the CLI is not on PATH (graceful fallback)
//   - forwards the event to the JSONL handler (when reachable); a failed or
//     hung sentra call never fails the user's git operation
//   - when delegatePath is non-empty, runs the repository's original hook
//     afterwards with the original arguments and propagates its exit status,
//     so flipping core.hooksPath never silently disables an existing hook
//   - never modifies the user's git working tree
//
// ShellShebang + SentinelHeader is the first line so we can identify our own
// hooks on rollback.
func renderScript(event, cliPath, delegatePath string) []byte {
	var b strings.Builder
	b.WriteString("#!/usr/bin/env bash\n")
	b.WriteString(SentinelHeader)
	b.WriteString("# Local-only hook installed by sentra-code-memory. Remove with:\n")
	b.WriteString("#   sentra-code-memory hooks uninstall --root <root>\n")
	b.WriteString("set -euo pipefail\n")
	b.WriteString("event=")
	b.WriteString(shellQuote(event))
	b.WriteString("\n")
	if cliPath != "" {
		b.WriteString("cli=")
		b.WriteString(shellQuote(cliPath))
		b.WriteString("\n")
	} else {
		b.WriteString("cli=\"$(command -v sentra-code-memory || true)\"\n")
	}
	b.WriteString("if [[ -n \"$cli\" ]]; then\n")
	b.WriteString("  \"$cli\" hooks run --event \"$event\" --root \"$(git rev-parse --show-toplevel)\" </dev/null || true\n")
	b.WriteString("fi\n")
	if delegatePath != "" {
		b.WriteString("prior=")
		b.WriteString(shellQuote(delegatePath))
		b.WriteString("\n")
		b.WriteString("if [[ -x \"$prior\" ]]; then\n")
		b.WriteString("  rc=0\n")
		b.WriteString("  \"$prior\" \"$@\" || rc=$?\n")
		b.WriteString("  exit \"$rc\"\n")
		b.WriteString("fi\n")
	}
	b.WriteString("exit 0\n")
	return []byte(b.String())
}

// renderPassthroughScript returns a sentinel-tagged script whose only job is
// to run the repository's original hook (for a kind this installer does not
// manage) so flipping core.hooksPath never disables it. When the original
// hook disappears the shim degrades to exit 0.
func renderPassthroughScript(priorPath string) []byte {
	var b strings.Builder
	b.WriteString("#!/usr/bin/env bash\n")
	b.WriteString(SentinelHeader)
	b.WriteString("# Passthrough hook installed by sentra-code-memory because core.hooksPath\n")
	b.WriteString("# points at .sentra/hooks. It delegates to the repository's original hook.\n")
	b.WriteString("# Remove with:\n")
	b.WriteString("#   sentra-code-memory hooks uninstall --root <root>\n")
	b.WriteString("set -euo pipefail\n")
	b.WriteString("prior=")
	b.WriteString(shellQuote(priorPath))
	b.WriteString("\n")
	b.WriteString("if [[ -x \"$prior\" ]]; then\n")
	b.WriteString("  exec \"$prior\" \"$@\"\n")
	b.WriteString("fi\n")
	b.WriteString("exit 0\n")
	return []byte(b.String())
}

// shellQuote produces a single-quoted bash literal.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'"
}

// hashContent returns the canonical SHA-256 of v (hex).
func hashContent(v []byte) string {
	sum := sha256.Sum256(v)
	return hex.EncodeToString(sum[:])
}

// loadManifest reads the manifest at hooksDir/../state/<SnapshotName>. A
// missing manifest is not an error: it returns (nil, false, nil). A corrupt
// manifest returns ErrManifestCorrupt so callers fail closed.
func loadManifest(stateDir string) (*Manifest, bool, error) {
	raw, exists, err := readFileIfExists(filepath.Join(stateDir, ManifestName))
	if err != nil {
		return nil, false, err
	}
	if !exists {
		return nil, false, nil
	}
	var m Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, false, fmt.Errorf("%w: %v", ErrManifestCorrupt, err)
	}
	return &m, true, nil
}

// writeManifest atomically writes the manifest to state dir.
func writeManifest(stateDir string, m Manifest) error {
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(stateDir, ".sentra-manifest-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer func() { _ = os.Remove(name) }()
	if _, err := tmp.Write(raw); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(name, filepath.Join(stateDir, ManifestName)); err != nil {
		return err
	}
	d, err := os.Open(stateDir)
	if err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}

// validateKind refuses anything outside the supported set; this prevents
// callers from getting exotic hook names through path traversal or command
// injection vectors.
func validateKind(k HookKind) error {
	switch k {
	case HookPostCommit, HookPostCheckout, HookPostMerge, HookPrePush:
		return nil
	default:
		return fmt.Errorf("%w: %q", ErrUnknownHookKind, string(k))
	}
}

// selectedKinds dedups and orders the install target. Empty opts.Hooks
// means "all supported kinds"; otherwise the caller pinned the subset.
func selectedKinds(opts Options) ([]HookKind, error) {
	if len(opts.Hooks) == 0 {
		return append([]HookKind(nil), AllHooks...), nil
	}
	seen := map[HookKind]bool{}
	out := make([]HookKind, 0, len(opts.Hooks))
	for _, k := range opts.Hooks {
		if err := validateKind(k); err != nil {
			return nil, err
		}
		if seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, k)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out, nil
}

// Install writes the lifecycle hooks for opts.Hooks (default AllHooks).
// The operation is idempotent: re-running with identical content is a no-op.
// The operation is atomic per-hook: any failure mid-install leaves the prior
// file (which we snapshotted into the manifest) intact.
// The operation is rollback-safe: uninstall consults the manifest to restore
// prior byte-for-byte state including file mode and existence.
func Install(opts Options) (Result, error) {
	root, err := ResolveRoot(opts)
	if err != nil {
		return Result{}, err
	}
	strategy, gitCommon, err := resolveStrategy(root, opts)
	if err != nil {
		return Result{}, err
	}
	mu := guardFor(root)
	mu.Lock()
	defer mu.Unlock()

	kinds, err := selectedKinds(opts)
	if err != nil {
		return Result{}, err
	}
	hooksDir, err := resolveHooksDir(root, strategy, gitCommon)
	if err != nil {
		return Result{}, err
	}
	if err := ensureConfined(hooksDir, allowedRootFor(strategy, root, gitCommon)); err != nil {
		return Result{}, err
	}
	stateDir := filepath.Join(filepath.Dir(hooksDir), StateDirName)

	manifest := Manifest{
		Schema:    ManifestSchema,
		Root:      root,
		Strategy:  strategy,
		HooksDir:  hooksDir,
		Installed: []InstalledHook{},
	}
	prevManifest, _, err := loadManifest(stateDir)
	if err != nil {
		return Result{}, err
	}
	prevByKind := map[HookKind]InstalledHook{}
	if prevManifest != nil {
		for _, entry := range prevManifest.Installed {
			prevByKind[entry.Kind] = entry
		}
	}

	var notes []string

	// Flipping core.hooksPath shadows whatever hooks git used to run. Before
	// any write, capture the prior config value and the prior hooks directory
	// so existing hooks keep working: managed kinds delegate to the prior
	// script, every other active hook gets a passthrough. Scan errors fail
	// closed.
	delegate := map[HookKind]string{}
	var shimNames []string
	trackConfig := strategy == StrategyRepoHooks && IsGitRepo(root)
	if trackConfig {
		priorLocal, err := currentHooksPath(root)
		if err != nil {
			return Result{}, err
		}
		priorHooksPath := priorLocal
		priorHooksPathSet := priorLocal != ""
		scanValue := priorLocal
		if scanValue == "" {
			scanValue = effectiveHooksPath(root)
		}
		// A re-install sees our own hooksPath in local config; inherit the
		// original prior state recorded by the first install so sequential
		// installs never lose the pre-first-install snapshot or rescan our
		// own hooks directory.
		if prevManifest != nil && prevManifest.HooksPath != "" && priorLocal != "" &&
			samePath(priorLocal, prevManifest.HooksPath) {
			priorHooksPath = prevManifest.PriorHooksPath
			priorHooksPathSet = prevManifest.PriorHooksPathSet
			if prevManifest.PriorHooksDir != "" {
				scanValue = prevManifest.PriorHooksDir
			}
		}
		common, err := gitCommonDir(root)
		if err != nil {
			return Result{}, err
		}
		priorDir := resolvePriorHooksDir(root, scanValue, common)
		active, err := scanActiveHooks(priorDir)
		if err != nil {
			return Result{}, fmt.Errorf("scan existing hooks in %s: %w", priorDir, err)
		}
		selected := map[HookKind]bool{}
		for _, k := range kinds {
			selected[k] = true
		}
		for _, name := range active {
			if selected[HookKind(name)] {
				delegate[HookKind(name)] = filepath.Join(priorDir, name)
				continue
			}
			shimNames = append(shimNames, name)
		}
		manifest.PriorHooksPath = priorHooksPath
		manifest.PriorHooksPathSet = priorHooksPathSet
		manifest.PriorHooksDir = priorDir
	}

	changed := false
	for _, k := range kinds {
		entry, wrote, err := installOne(hooksDir, k, opts, strategy, prevByKind, delegate[k])
		if err != nil {
			return Result{}, fmt.Errorf("%s: %w", k, err)
		}
		changed = changed || wrote
		manifest.Installed = append(manifest.Installed, entry)
		if entry.PriorExisted {
			notes = append(notes, fmt.Sprintf("%s: replaced prior file (%d bytes); snapshot preserved", k, len(entry.PriorSnapshot)))
		} else {
			notes = append(notes, fmt.Sprintf("%s: installed (no prior file at target path)", k))
		}
		if delegate[k] != "" {
			notes = append(notes, fmt.Sprintf("%s: delegates to existing hook at %s", k, delegate[k]))
		}
	}
	for _, name := range shimNames {
		content := renderPassthroughScript(filepath.Join(manifest.PriorHooksDir, name))
		prevEntry, hasPrev := prevByKind[HookKind(name)]
		entry, wrote, err := installScript(hooksDir, name, content, prevEntry, hasPrev, strategy)
		if err != nil {
			return Result{}, fmt.Errorf("%s: %w", name, err)
		}
		changed = changed || wrote
		manifest.Installed = append(manifest.Installed, entry)
		notes = append(notes, fmt.Sprintf(
			"%s: passthrough hook preserves existing hook at %s", name, filepath.Join(manifest.PriorHooksDir, name)))
	}

	// Carry forward manifest entries this run did not rewrite so sequential
	// subset installs never lose a hook or its prior snapshot.
	owned := map[HookKind]bool{}
	for _, entry := range manifest.Installed {
		owned[entry.Kind] = true
	}
	if prevManifest != nil {
		for _, entry := range prevManifest.Installed {
			if owned[entry.Kind] {
				continue
			}
			manifest.Installed = append(manifest.Installed, entry)
			owned[entry.Kind] = true
		}
	}
	sortInstalled(manifest.Installed)

	// For repo-hooks strategy, point core.hooksPath at the local .sentra/hooks
	// directory so subsequent git operations on this checkout run our hooks.
	if trackConfig {
		actual, err := currentHooksPath(root)
		if err != nil {
			return Result{}, err
		}
		if actual == "" || !samePath(actual, hooksDir) {
			hooksPath, err := setLocalHooksPath(root, hooksDir)
			if err != nil {
				return Result{}, err
			}
			manifest.HooksPath = hooksPath
			changed = true
			notes = append(notes, "set git config core.hooksPath to "+hooksPath)
		} else {
			manifest.HooksPath = actual
		}
	}

	manifest.CLIExecutable = opts.CLIExecutable
	manifest.InstalledAt = nowUTC()
	if prevManifest != nil && !changed &&
		manifestSignature(*prevManifest) == manifestSignature(manifest) {
		// True no-op install: keep the original timestamp so repeated
		// installs produce a byte-identical manifest.
		manifest.InstalledAt = prevManifest.InstalledAt
	}
	if err := writeManifest(stateDir, manifest); err != nil {
		return Result{}, err
	}
	return Result{OK: true, Action: "install", Strategy: strategy, Manifest: manifest, Notes: notes}, nil
}

// installOne renders and writes the script for one managed hook kind. The
// delegatePath (possibly empty) points at a pre-existing hook script that
// the installed script must keep running after the sentra event.
func installOne(hooksDir string, k HookKind, opts Options, strategy Strategy, prev map[HookKind]InstalledHook, delegatePath string) (InstalledHook, bool, error) {
	content := renderScript(string(k), opts.CLIExecutable, delegatePath)
	prevEntry, hasPrev := prev[k]
	return installScript(hooksDir, string(k), content, prevEntry, hasPrev, strategy)
}

// installScript atomically writes one hook script (managed or passthrough)
// to hooksDir/name with mode 0755. It captures any prior file for rollback,
// inherits the original snapshot when reinstalling over our own
// sentinel-tagged script, and skips the write entirely when the live file
// already matches (so repeated installs never bump mtimes). The boolean
// return reports whether a write occurred.
func installScript(hooksDir, name string, content []byte, prevEntry InstalledHook, hasPrev bool, strategy Strategy) (InstalledHook, bool, error) {
	target := filepath.Join(hooksDir, name)
	entry := InstalledHook{
		Kind:       HookKind(name),
		Path:       target,
		ContentSHA: hashContent(content),
		Mode:       "0755",
	}
	priorData, priorExists, err := readFileIfExists(target)
	if err != nil {
		return entry, false, err
	}
	if priorExists {
		// For StrategyGitCommon we share git's hooks directory with other
		// tools. Refuse to overwrite a non-sentra hook so we never destroy
		// another tool's contract. For StrategyRepoHooks we own the
		// .sentra directory outright and capturing any prior file is safe.
		if strategy == StrategyGitCommon && !bytesHaveSentinel(priorData) {
			return entry, false, fmt.Errorf("%w: %s is not a sentra-installed hook", ErrInstalledByOther, target)
		}
		// Idempotent: when the live file content already matches what we
		// would write, the install is a no-op and we report whatever the
		// previous manifest recorded. This preserves the "pre-first-install"
		// snapshot through repeated identical installs, even after the first
		// install placed a sentra hook in the slot.
		if hashContent(priorData) == entry.ContentSHA {
			if hasPrev {
				return prevEntry, false, nil
			}
			// No previous manifest entry: keep the live file's metadata
			// so uninstall will still restore byte-for-byte.
			entry.PriorExisted = true
			entry.PriorPath = target
			entry.PriorSnapshot = string(priorData)
			entry.PriorMode = fileModeString(target)
			return entry, false, nil
		}
		if bytesHaveSentinel(priorData) && hasPrev {
			// Reinstalling our own hook with different content (e.g. a new
			// CLI path): inherit the pre-first-install snapshot so the
			// original prior state survives sequential installs instead of
			// being replaced by a snapshot of our own earlier script.
			entry.PriorExisted = prevEntry.PriorExisted
			entry.PriorPath = prevEntry.PriorPath
			entry.PriorSnapshot = prevEntry.PriorSnapshot
			entry.PriorMode = prevEntry.PriorMode
		} else {
			// Live file exists but differs from what we would write; capture
			// its current state as the prior so uninstall restores
			// byte-for-byte.
			entry.PriorExisted = true
			entry.PriorPath = target
			entry.PriorSnapshot = string(priorData)
			entry.PriorMode = fileModeString(target)
		}
	}
	if err := ensureConfined(target, filepath.Dir(hooksDir)); err != nil {
		return entry, false, err
	}
	if err := writeHookFile(target, content, 0o755); err != nil {
		return entry, false, err
	}
	return entry, true, nil
}

// fileModeString returns the octal permission string (e.g. "0755") of path,
// or "" when the file cannot be stat'ed.
func fileModeString(path string) string {
	if info, err := os.Stat(path); err == nil {
		return fmt.Sprintf("%04o", info.Mode().Perm())
	}
	return ""
}

// sortInstalled orders manifest entries deterministically: managed kinds in
// AllHooks order first, passthrough/foreign names alphabetically after.
func sortInstalled(list []InstalledHook) {
	order := func(name string) (int, string) {
		for i, k := range AllHooks {
			if string(k) == name {
				return i, ""
			}
		}
		return len(AllHooks), name
	}
	sort.SliceStable(list, func(i, j int) bool {
		oi, ni := order(string(list[i].Kind))
		oj, nj := order(string(list[j].Kind))
		if oi != oj {
			return oi < oj
		}
		return ni < nj
	})
}

// manifestSignature summarizes every state-bearing manifest field except the
// timestamp so no-op installs can be detected deterministically.
func manifestSignature(m Manifest) string {
	var b strings.Builder
	b.WriteString(string(m.Strategy))
	b.WriteByte('|')
	b.WriteString(m.HooksPath)
	for _, e := range m.Installed {
		b.WriteByte('|')
		b.WriteString(string(e.Kind))
		b.WriteByte(':')
		b.WriteString(e.ContentSHA)
	}
	return b.String()
}

// bytesHaveSentinel reports whether buf contains the sentinel header
// identifying sentra-managed hooks. The header lives after the #! shebang
// line, so the check is "contains anywhere" rather than "begins with".
func bytesHaveSentinel(buf []byte) bool {
	return strings.Contains(string(buf), SentinelHeader)
}

// Uninstall restores every hook in the manifest to its prior state. Hooks
// that never had a prior file are removed. After uninstall, the manifest
// file itself is removed so a subsequent install starts from scratch (no
// stale prior state can leak across runs).
func Uninstall(opts Options) (Result, error) {
	root, err := ResolveRoot(opts)
	if err != nil {
		return Result{}, err
	}
	strategy, gitCommon, err := resolveStrategy(root, opts)
	if err != nil {
		return Result{}, err
	}
	mu := guardFor(root)
	mu.Lock()
	defer mu.Unlock()

	hooksDir, err := resolveHooksDir(root, strategy, gitCommon)
	if err != nil {
		return Result{}, err
	}
	stateDir := filepath.Join(filepath.Dir(hooksDir), StateDirName)
	manifest, exists, err := loadManifest(stateDir)
	if err != nil {
		return Result{}, err
	}
	if !exists {
		return Result{OK: true, Action: "uninstall", Strategy: strategy,
			Manifest: Manifest{Schema: ManifestSchema, Root: root, Strategy: strategy},
			Notes:    []string{"nothing to uninstall (no manifest found)"}}, nil
	}

	var notes []string
	for _, entry := range manifest.Installed {
		target := entry.Path
		if target == "" {
			notes = append(notes, fmt.Sprintf("%s: skipped (manifest entry has no path)", entry.Kind))
			continue
		}
		if err := ensureConfined(target, filepath.Dir(hooksDir)); err != nil {
			return Result{}, err
		}
		live, liveExists, err := readFileIfExists(target)
		if err != nil {
			return Result{}, fmt.Errorf("%s: %w", entry.Kind, err)
		}
		if liveExists && !bytesHaveSentinel(live) {
			// The live file is no longer sentra-managed (the user or another
			// tool replaced it). Never destroy a file we do not own.
			notes = append(notes, fmt.Sprintf("%s: skipped (live file is not sentra-managed)", entry.Kind))
			continue
		}
		if !entry.PriorExisted {
			if err := os.Remove(target); err != nil && !errors.Is(err, os.ErrNotExist) {
				return Result{}, fmt.Errorf("%s: remove: %w", entry.Kind, err)
			}
			notes = append(notes, fmt.Sprintf("%s: removed (no prior file)", entry.Kind))
			continue
		}
		mode := os.FileMode(0o755)
		if entry.PriorMode != "" {
			fmt.Sscanf(entry.PriorMode, "%o", &mode)
		}
		if err := writeHookFile(target, []byte(entry.PriorSnapshot), mode); err != nil {
			return Result{}, fmt.Errorf("%s: restore: %w", entry.Kind, err)
		}
		notes = append(notes, fmt.Sprintf("%s: restored prior file (%d bytes)", entry.Kind, len(entry.PriorSnapshot)))
	}

	// Restore core.hooksPath when it still points at the hooks dir we
	// installed: write back the prior local value recorded in the manifest,
	// or unset the setting when there was none. A value the user changed
	// after the install is left untouched.
	if strategy == StrategyRepoHooks && manifest.HooksPath != "" && IsGitRepo(root) {
		actual, err := currentHooksPath(root)
		if err != nil {
			return Result{}, err
		}
		if actual != "" && samePath(actual, manifest.HooksPath) {
			if manifest.PriorHooksPathSet {
				if _, err := setLocalHooksPath(root, manifest.PriorHooksPath); err != nil {
					return Result{}, err
				}
				notes = append(notes, "restored prior git config core.hooksPath ("+manifest.PriorHooksPath+")")
			} else {
				if err := unsetLocalHooksPath(root); err != nil {
					return Result{}, err
				}
				notes = append(notes, "cleared git config core.hooksPath")
			}
		} else {
			notes = append(notes, "core.hooksPath does not match the installed hooks dir; left unchanged")
		}
	}

	// Remove the manifest after a successful uninstall. Any subsequent
	// install will start with a fresh snapshot.
	manifestPath := filepath.Join(stateDir, ManifestName)
	if err := os.Remove(manifestPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return Result{}, err
	}
	manifest.Installed = manifest.Installed[:0]
	return Result{OK: true, Action: "uninstall", Strategy: strategy,
		Manifest: *manifest, Notes: notes}, nil
}

// Status reports the current state of the install without mutating it. The
// Manifest field is populated only when one is on disk; Unexpected lists any
// sentinel-tagged hook files in the hooks directory that the live manifest
// does not own (e.g. hand-installed previously with a different run).
func Status(opts Options) (Report, error) {
	root, err := ResolveRoot(opts)
	if err != nil {
		return Report{}, err
	}
	strategy, gitCommon, err := resolveStrategy(root, opts)
	if err != nil {
		return Report{}, err
	}
	hooksDir, err := resolveHooksDir(root, strategy, gitCommon)
	if err != nil {
		return Report{}, err
	}
	stateDir := filepath.Join(filepath.Dir(hooksDir), StateDirName)
	s := Report{
		OK:        true,
		Schema:    ManifestSchema,
		Root:      root,
		Strategy:  strategy,
		HooksDir:  hooksDir,
		Installed: []string{},
		Missing:   []string{},
	}
	manifest, exists, err := loadManifest(stateDir)
	if err != nil {
		return Report{}, err
	}
	if !exists {
		s.Notes = append(s.Notes, "no manifest found; install to create one")
		s.HooksPath, _ = currentHooksPath(root)
		return s, nil
	}
	s.Manifest = *manifest
	s.HooksPath, _ = currentHooksPath(root)

	ownedByKind := map[HookKind]bool{}
	for _, e := range manifest.Installed {
		ownedByKind[e.Kind] = true
		data, fileExists, ferr := readFileIfExists(e.Path)
		if ferr != nil {
			return Report{}, ferr
		}
		if !fileExists || hashContent(data) != e.ContentSHA {
			s.Missing = append(s.Missing, string(e.Kind))
		} else {
			s.Installed = append(s.Installed, string(e.Kind))
		}
	}

	// Walk the hooks directory and identify any sentinel-tagged script that
	// is NOT in the manifest — a safe observation that never modifies state.
	entries, err := os.ReadDir(hooksDir)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return Report{}, err
	}
	for _, ent := range entries {
		if ent.IsDir() {
			continue
		}
		k := HookKind(ent.Name())
		if ownedByKind[k] {
			continue
		}
		data, _ := os.ReadFile(filepath.Join(hooksDir, ent.Name()))
		if bytesHaveSentinel(data) {
			s.Unexpected = append(s.Unexpected, ent.Name())
		}
	}
	return s, nil
}

// resolveStrategy returns the resolved strategy + git common directory and
// performs the safety checks. StrategyGitCommon requires AllowUnsafeGitCommon
// AND IsGitRepo(root).
func resolveStrategy(root string, opts Options) (Strategy, string, error) {
	strat := opts.Strategy
	if strat == "" {
		strat = StrategyRepoHooks
	}
	switch strat {
	case StrategyRepoHooks:
		return strat, "", nil
	case StrategyGitCommon:
		if !opts.AllowUnsafeGitCommon {
			return "", "", ErrUnsafeGitCommon
		}
		if !IsGitRepo(root) {
			return "", "", ErrNotGitRepo
		}
		common, err := gitCommonDir(root)
		if err != nil {
			return "", "", err
		}
		return strat, common, nil
	default:
		return "", "", fmt.Errorf("lifecycle: unknown strategy %q", strat)
	}
}

// allowedRootFor returns the directory under which writeHookFile targets
// must be confined. Any path that escapes this root via symlinks is refused
// before the write.
func allowedRootFor(strategy Strategy, root, gitCommon string) string {
	switch strategy {
	case StrategyRepoHooks:
		return filepath.Join(root, ".sentra")
	case StrategyGitCommon:
		return gitCommon
	default:
		return "/"
	}
}

// setLocalHooksPath runs `git config --local core.hooksPath <dir>` for root
// and returns the value git now reports. The invocation uses the same
// sandboxed flags as gitCommonDir to prevent recursion into installed hooks.
func setLocalHooksPath(root, dir string) (string, error) {
	cmd := exec.Command("git", "-C", root,
		"-c", "core.hooksPath=/dev/null",
		"-c", "core.fsmonitor=false",
		"-c", "credential.helper=",
		"config", "--local", "core.hooksPath", dir)
	cmd.Env = []string{"HOME=/nonexistent", "LANG=C", "PATH=" + os.Getenv("PATH")}
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("git config core.hooksPath: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return currentHooksPath(root)
}

// unsetLocalHooksPath removes the local core.hooksPath setting. Callers
// verify beforehand that the live value is one this installer wrote.
func unsetLocalHooksPath(root string) error {
	cmd := exec.Command("git", "-C", root,
		"-c", "core.hooksPath=/dev/null",
		"-c", "core.fsmonitor=false",
		"-c", "credential.helper=",
		"config", "--local", "--unset-all", "core.hooksPath")
	cmd.Env = []string{"HOME=/nonexistent", "LANG=C", "PATH=" + os.Getenv("PATH")}
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git config --unset core.hooksPath: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// currentHooksPath returns `git config --local core.hooksPath` for root.
// Empty (no error) means unset in the local scope.
func currentHooksPath(root string) (string, error) {
	if !IsGitRepo(root) {
		return "", nil
	}
	cmd := exec.Command("git", "-C", root,
		"-c", "core.fsmonitor=false",
		"config", "--local", "--get", "core.hooksPath")
	cmd.Env = []string{"HOME=/nonexistent", "LANG=C", "PATH=" + os.Getenv("PATH")}
	out, err := cmd.Output()
	if err != nil {
		return "", nil // exit code 1 == unset, treat as empty
	}
	return strings.TrimSpace(string(out)), nil
}

// effectiveHooksPath returns core.hooksPath as git resolves it across the
// system/global/local scopes. Read-only; unlike the sandboxed installer
// commands it inherits the caller's HOME (and GIT_CONFIG_* overrides when
// set) so inherited config is visible. Empty means unset everywhere.
func effectiveHooksPath(root string) string {
	if !IsGitRepo(root) {
		return ""
	}
	cmd := exec.Command("git", "-C", root,
		"-c", "core.fsmonitor=false",
		"config", "--get", "core.hooksPath")
	env := []string{
		"HOME=" + os.Getenv("HOME"),
		"LANG=C",
		"PATH=" + os.Getenv("PATH"),
		"GIT_TERMINAL_PROMPT=0",
	}
	for _, key := range []string{"GIT_CONFIG_GLOBAL", "GIT_CONFIG_SYSTEM"} {
		if v := os.Getenv(key); v != "" {
			env = append(env, key+"="+v)
		}
	}
	cmd.Env = env
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// resolvePriorHooksDir returns the directory git reads hooks from before the
// installer flips core.hooksPath. An empty hooksPathValue means git's
// default <git-common-dir>/hooks. Relative hooksPath values are resolved
// against root, matching hook invocation from the working tree root.
func resolvePriorHooksDir(root, hooksPathValue, gitCommon string) string {
	if hooksPathValue == "" {
		if gitCommon == "" {
			return ""
		}
		return filepath.Join(gitCommon, HooksDirName)
	}
	if filepath.IsAbs(hooksPathValue) {
		return filepath.Clean(hooksPathValue)
	}
	return filepath.Join(root, hooksPathValue)
}

// scanActiveHooks lists the hook files in dir that git would execute:
// executable regular files (or symlinks) whose names are not *.sample,
// dotfiles, or already sentinel-tagged sentra hooks. Sorted names. A
// missing directory yields an empty list; an unreadable one fails closed.
func scanActiveHooks(dir string) ([]string, error) {
	if dir == "" {
		return nil, nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var out []string
	for _, ent := range entries {
		name := ent.Name()
		if strings.HasPrefix(name, ".") || strings.HasSuffix(name, ".sample") {
			continue
		}
		info, err := ent.Info()
		if err != nil {
			return nil, err
		}
		mode := info.Mode()
		if mode.IsDir() {
			continue
		}
		if mode&os.ModeSymlink == 0 && (!mode.IsRegular() || mode.Perm()&0o111 == 0) {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			return nil, err
		}
		if bytesHaveSentinel(data) {
			continue
		}
		out = append(out, name)
	}
	sort.Strings(out)
	return out, nil
}

func samePath(a, b string) bool {
	ar, _ := filepath.EvalSymlinks(a)
	br, _ := filepath.EvalSymlinks(b)
	if ar == "" {
		ar = a
	}
	if br == "" {
		br = b
	}
	return ar == br
}

// RunHook is the entry point installed scripts call. It is a no-op currently
// — it exists so the hook can forward events to the CLI without the CLI
// having to embed bash logic — and it is a future extension point for
// agents wanting lifecycle event delivery. Network use is forbidden here;
// keep events local.
func RunHook(event, root string) error {
	if err := validateKind(HookKind(event)); err != nil {
		// Unknown hook kinds are not installer failures; they belong to
		// other tools. Return nil so the calling git workflow is undisturbed.
		return nil
	}
	if root == "" {
		var err error
		root, err = os.Getwd()
		if err != nil {
			return nil
		}
	}
	if !IsGitRepo(root) {
		return nil
	}
	// The hook intentionally does not perform any action beyond exiting 0.
	// Sentra lifecycle events are observable through the watch/refresh
	// pipeline, so duplicating them here would be redundant. The CLI
	// subcommand exists so installed scripts have a stable command line.
	return nil
}

// nowUTC returns the canonical RFC3339 UTC timestamp used in the manifest.
// Indirected via a closure so tests can override the clock without depending
// on the system time directly.
var nowFunc = func() time.Time { return time.Now().UTC() }

func nowUTC() string {
	return nowFunc().Format(time.RFC3339)
}
