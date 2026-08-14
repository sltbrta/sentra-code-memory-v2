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
	// effect for subsequent git operations on this checkout. Reversed by
	// uninstall.
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
	// reviewer approval because it never writes into .git/.
	Strategy Strategy
	// Hooks restricts the install/uninstall set. When empty, AllHooks is
	// used. Status ignores Hooks and reports every installed hook.
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
	// when Strategy != StrategyRepoHooks. Uninstall clears it.
	HooksPath     string `json:"hooks_path,omitempty"`
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
//   - forwards the event to the JSONL handler (when reachable) with a strict
//     timeout so a hung CLI cannot stall commit/checkout operations
//   - never modifies the user's git working tree
//
// ShellShebang + SentinelHeader is the first line so we can identify our own
// hooks on rollback.
func renderScript(event string, cliPath string) []byte {
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
	b.WriteString("if [[ -z \"$cli\" ]]; then\n  exit 0\nfi\n")
	b.WriteString("\"$cli\" hooks run --event \"$event\" --root \"$(git rev-parse --show-toplevel)\" </dev/null || exit 0\n")
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
	for _, k := range kinds {
		entry, err := installOne(root, hooksDir, k, opts, strategy, prevByKind)
		if err != nil {
			return Result{}, fmt.Errorf("%s: %w", k, err)
		}
		manifest.Installed = append(manifest.Installed, entry)
		if entry.PriorExisted {
			notes = append(notes, fmt.Sprintf("%s: replaced prior file (%d bytes); snapshot preserved", k, len(entry.PriorSnapshot)))
		} else {
			notes = append(notes, fmt.Sprintf("%s: installed (no prior file at target path)", k))
		}
	}

	// For repo-hooks strategy, set core.hooksPath to the local .sentra/hooks
	// path so subsequent git operations on this checkout run our hooks.
	if strategy == StrategyRepoHooks && IsGitRepo(root) {
		hooksPath, err := setHooksPath(root, hooksDir)
		if err != nil {
			return Result{}, err
		}
		manifest.HooksPath = hooksPath
		notes = append(notes, "set git config core.hooksPath to "+hooksPath)
	}

	manifest.InstalledAt = nowUTC()
	manifest.CLIExecutable = opts.CLIExecutable
	if err := writeManifest(stateDir, manifest); err != nil {
		return Result{}, err
	}
	return Result{OK: true, Action: "install", Strategy: strategy, Manifest: manifest, Notes: notes}, nil
}

// installOne handles a single hook kind: capture prior state, render the
// script, atomic-write it with mode 0755. Idempotency skips the write when
// the rendered content already matches the live file (so repeated installs
// never bump mtimes).
func installOne(root, hooksDir string, k HookKind, opts Options, strategy Strategy, prev map[HookKind]InstalledHook) (InstalledHook, error) {
	target := filepath.Join(hooksDir, string(k))
	content := renderScript(string(k), opts.CLIExecutable)
	entry := InstalledHook{
		Kind:       k,
		Path:       target,
		ContentSHA: hashContent(content),
		Mode:       "0755",
	}
	priorData, priorExists, err := readFileIfExists(target)
	if err != nil {
		return entry, err
	}
	if priorExists {
		// For StrategyGitCommon we share git's hooks directory with other
		// tools. Refuse to overwrite a non-sentra hook so we never destroy
		// another tool's contract. For StrategyRepoHooks we own the
		// .sentra directory outright and capturing any prior file is safe.
		if strategy == StrategyGitCommon && !bytesHaveSentinel(priorData) {
			return entry, fmt.Errorf("%w: %s is not a sentra-installed hook", ErrInstalledByOther, target)
		}
		// Idempotent: when the live file content already matches what we
		// would write, the install is a no-op and we report whatever the
		// previous manifest recorded. This preserves the "pre-first-install"
		// snapshot through repeated identical installs, even after the first
		// install placed a sentra hook in the slot.
		if hashContent(priorData) == entry.ContentSHA {
			if prevEntry, ok := prev[k]; ok {
				return prevEntry, nil
			}
			// No previous manifest entry: keep the live file's metadata
			// so uninstall will still restore byte-for-byte.
			entry.PriorExisted = true
			entry.PriorPath = target
			entry.PriorSnapshot = string(priorData)
			if info, statErr := os.Stat(target); statErr == nil {
				entry.PriorMode = fmt.Sprintf("%04o", info.Mode().Perm())
			}
			return entry, nil
		}
		// Live file exists but differs from what we would write; capture
		// its current state as the prior so uninstall restores byte-for-byte.
		entry.PriorExisted = true
		entry.PriorPath = target
		entry.PriorSnapshot = string(priorData)
		if info, statErr := os.Stat(target); statErr == nil {
			entry.PriorMode = fmt.Sprintf("%04o", info.Mode().Perm())
		}
	}
	if err := ensureConfined(target, filepath.Dir(hooksDir)); err != nil {
		return entry, err
	}
	if err := writeHookFile(target, content, 0o755); err != nil {
		return entry, err
	}
	return entry, nil
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
		if err := ensureConfined(target, filepath.Dir(hooksDir)); err != nil {
			return Result{}, err
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

	// Clear core.hooksPath if we set it ourselves (only repo-hooks strategy
	// touches git config; verified manifest.HooksPath is non-empty).
	if strategy == StrategyRepoHooks && manifest.HooksPath != "" && IsGitRepo(root) {
		if err := clearHooksPath(root, manifest.HooksPath); err != nil {
			return Result{}, err
		}
		notes = append(notes, "cleared git config core.hooksPath")
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

// setHooksPath runs `git config --local core.hooksPath <dir>` for root. The
// invocation uses the same sandboxed flags as gitCommonDir to prevent
// recursion into installed hooks.
func setHooksPath(root, dir string) (string, error) {
	absDir, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	cmd := exec.Command("git", "-C", root,
		"-c", "core.hooksPath=/dev/null",
		"-c", "core.fsmonitor=false",
		"-c", "credential.helper=",
		"config", "--local", "core.hooksPath", absDir)
	cmd.Env = []string{"HOME=/nonexistent", "LANG=C", "PATH=" + os.Getenv("PATH")}
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("git config core.hooksPath: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return currentHooksPath(root)
}

// clearHooksPath removes the local core.hooksPath setting when it matches
// the path we installed. We avoid clearing an unrelated value the user set
// by themselves.
func clearHooksPath(root, expected string) error {
	actual, err := currentHooksPath(root)
	if err != nil {
		return nil // nothing to do; better a stale install removed than a stale config left
	}
	if !samePath(actual, expected) {
		return nil
	}
	cmd := exec.Command("git", "-C", root,
		"-c", "core.hooksPath=/dev/null",
		"-c", "core.fsmonitor=false",
		"-c", "credential.helper=",
		"config", "--local", "--unset", "core.hooksPath")
	cmd.Env = []string{"HOME=/nonexistent", "LANG=C", "PATH=" + os.Getenv("PATH")}
	_, _ = cmd.CombinedOutput() // best effort; the manifest removal is the source of truth
	return nil
}

// currentHooksPath returns `git config --local core.hooksPath` for root.
// Empty (no error) means unset.
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
