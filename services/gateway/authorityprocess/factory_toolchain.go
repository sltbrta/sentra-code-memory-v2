package authorityprocess

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	broker "github.com/sltbrta/sentra-code-memory-v2/services/broker/localauthority"
)

// The BUILD and TEST gates did not build or test.
//
// BUILD checked that every leaf reached COMPLETED -- a property of the
// executor, not of the code it produced. TEST checked that touched Go files
// parse, which is a strictly weaker claim than compiling: an undefined symbol,
// a type error, a wrong signature and a missing import all parse. A change set
// that touched no Go file at all skipped both and passed having been checked
// by nothing. Callers read FACTORY_GATE_STATUS_PASSED as an assurance that the
// change set builds and its tests pass, and it never was one.
//
// The obstacle was structural rather than a bodge away: the gate receives the
// leaf's in-memory post-image bytes and had neither a repository root nor
// anywhere to run. A package does not compile in isolation from its module, so
// there was nothing for `go build` to be pointed at. Compiling the fragment
// and calling the result a build would have been the same overclaiming in a
// new form.
//
// The route through that is `go build -overlay`: the toolchain accepts a JSON
// file mapping real paths to replacements, so the candidate tree can be
// compiled against the real module without the edits ever touching it. The
// edits stay in a temporary directory, the repository is not modified, and
// what is compiled is exactly what the leaf produced.
//
// The child runs under the same discipline as the change-set verification gate
// (workflow/verification_command.go): a fixed argv with no shell, a scrubbed
// and offline environment, a deadline, and a process-group kill so a test
// runner's workers die with it.
//
// Two limits of the overlay mechanism, both measured rather than assumed:
//
//   - An edit to go.mod is not seen. The module is loaded before the overlay
//     is consulted, so a candidate that changes requirements is compiled
//     against the module as it is on disk. A change set that edits go.mod is
//     therefore gated on everything except the thing it changed.
//   - `go build` does not compile test files, so a package left holding only
//     its tests builds cleanly. That is why BUILD is not treated as a superset
//     of TEST and both are run.
//
// Overlay keys are matched against paths the go command has itself resolved,
// so the repository root is canonicalised before they are built. An
// unresolved key matches nothing and the overlay is silently ignored -- the
// gate would then compile the original tree and report a pass on the
// candidate, which is precisely the failure this change exists to remove.

// factoryToolchainTimeout bounds one gate invocation. `go test ./...` over a
// module of this size is minutes, not seconds, and a gate that times out is
// recorded as a failure -- so the bound is generous enough that only a genuine
// hang reaches it.
const factoryToolchainTimeout = 10 * time.Minute

// errNoToolchain reports a BUILD or TEST gate asked to run with no repository
// root configured. It is an error rather than a pass: a gate that cannot check
// anything must not report that it checked.
var errNoToolchain = errors.New("authorityprocess: factory build gate has no repository root")

// factoryToolchain compiles and tests a candidate change set against the real
// module, without writing into it.
type factoryToolchain struct {
	// repoRoot is the approved source root: the module the edits belong to.
	repoRoot string
	// goBin is the toolchain binary. Empty means look it up on PATH.
	goBin string
	// timeout bounds one invocation; zero means factoryToolchainTimeout.
	timeout time.Duration
}

// configured reports whether this toolchain can run anything.
func (t factoryToolchain) configured() bool {
	return strings.TrimSpace(t.repoRoot) != ""
}

// canonicalRoot resolves repoRoot through its symlinks.
//
// Overlay keys are matched against the paths the go command itself resolves,
// and the go command resolves symlinks. On a host where the repository sits
// under one -- /var/folders on darwin is a link to /private/var/folders, which
// is where every temporary directory lands -- an unresolved key matches
// nothing, the overlay is silently ignored, and the gate compiles the
// original tree while reporting on the candidate. It passes, which is the
// failure mode this whole change exists to remove.
func (t factoryToolchain) canonicalRoot() (string, error) {
	root, err := filepath.Abs(strings.TrimSpace(t.repoRoot))
	if err != nil {
		return "", fmt.Errorf("authorityprocess: resolve repository root: %w", err)
	}
	resolved, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", fmt.Errorf("authorityprocess: resolve repository root: %w", err)
	}
	return resolved, nil
}

// Build compiles every package in the module with the candidate edits applied.
func (t factoryToolchain) Build(ctx context.Context, edits []broker.FactoryAppliedEdit) error {
	return t.run(ctx, edits, "build", "./...")
}

// Test compiles and runs every package's tests with the candidate edits
// applied. -count=1 defeats the test cache: a cached PASS recorded before the
// edits existed is not evidence about them.
func (t factoryToolchain) Test(ctx context.Context, edits []broker.FactoryAppliedEdit) error {
	return t.run(ctx, edits, "test", "-count=1", "./...")
}

// run materialises edits into an overlay and invokes one go subcommand.
func (t factoryToolchain) run(ctx context.Context, edits []broker.FactoryAppliedEdit, args ...string) error {
	if !t.configured() {
		return errNoToolchain
	}
	root, err := t.canonicalRoot()
	if err != nil {
		return err
	}
	overlay, cleanup, err := t.writeOverlay(edits)
	if err != nil {
		return err
	}
	defer cleanup()

	timeout := t.timeout
	if timeout <= 0 {
		timeout = factoryToolchainTimeout
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	goBin := t.goBin
	if goBin == "" {
		found, lookErr := exec.LookPath("go")
		if lookErr != nil {
			return fmt.Errorf("authorityprocess: locate go toolchain: %w", lookErr)
		}
		goBin = found
	}

	argv := append([]string{args[0], "-overlay", overlay}, args[1:]...)
	cmd := exec.CommandContext(runCtx, goBin, argv...)
	cmd.Dir = root
	cmd.Env = factoryToolchainEnv()
	// Own process group, signalled on timeout: exec.CommandContext kills only
	// the direct child, and every test runner spawns workers that would
	// otherwise outlive the deadline holding the overlay open.
	configureFactoryProcessGroup(cmd)
	cmd.Cancel = func() error { return killFactoryProcessGroup(cmd) }

	output, runErr := cmd.CombinedOutput()
	if runErr == nil {
		return nil
	}
	if runCtx.Err() != nil {
		return fmt.Errorf("authorityprocess: go %s timed out after %s", args[0], timeout)
	}
	// The output is diagnostic for the operator and never returned to the
	// caller: gate denials are non-disclosing by contract.
	return fmt.Errorf("authorityprocess: go %s failed: %w (%s)",
		args[0], runErr, truncateToolchainOutput(output))
}

// writeOverlay materialises each edit's post-image into a temporary directory
// and returns the path of an overlay file mapping the real paths onto them.
//
// A deleted or renamed-away path maps to the empty string, which the toolchain
// reads as "this file does not exist" -- so a rename is a deletion of the old
// path and a creation of the new one, and a change set that removes a file is
// compiled without it rather than with it.
func (t factoryToolchain) writeOverlay(edits []broker.FactoryAppliedEdit) (string, func(), error) {
	dir, err := os.MkdirTemp("", "factory-overlay-")
	if err != nil {
		return "", func() {}, fmt.Errorf("authorityprocess: overlay directory: %w", err)
	}
	cleanup := func() { _ = os.RemoveAll(dir) }

	replace := map[string]string{}
	for i, edit := range edits {
		target, err := t.resolveEditPath(edit.Path)
		if err != nil {
			cleanup()
			return "", func() {}, err
		}
		if edit.OldPath != "" && edit.OldPath != edit.Path {
			oldTarget, err := t.resolveEditPath(edit.OldPath)
			if err != nil {
				cleanup()
				return "", func() {}, err
			}
			replace[oldTarget] = ""
		}
		if strings.EqualFold(edit.Op, "delete") {
			replace[target] = ""
			continue
		}
		staged := filepath.Join(dir, fmt.Sprintf("%04d-%s", i, filepath.Base(edit.Path)))
		if err := os.WriteFile(staged, edit.AfterBytes, 0o600); err != nil {
			cleanup()
			return "", func() {}, fmt.Errorf("authorityprocess: stage overlay file: %w", err)
		}
		replace[target] = staged
	}

	raw, err := json.Marshal(struct {
		Replace map[string]string
	}{Replace: replace})
	if err != nil {
		cleanup()
		return "", func() {}, fmt.Errorf("authorityprocess: encode overlay: %w", err)
	}
	overlay := filepath.Join(dir, "overlay.json")
	if err := os.WriteFile(overlay, raw, 0o600); err != nil {
		cleanup()
		return "", func() {}, fmt.Errorf("authorityprocess: write overlay: %w", err)
	}
	return overlay, cleanup, nil
}

// resolveEditPath turns a repository-relative edit path into an absolute one
// inside the module, refusing anything that escapes it. The gate compiles what
// the path names, so a path that leaves the repository would compile a file
// the change set does not own.
func (t factoryToolchain) resolveEditPath(rel string) (string, error) {
	rel = strings.TrimSpace(rel)
	if rel == "" {
		return "", fmt.Errorf("authorityprocess: empty edit path")
	}
	if filepath.IsAbs(rel) {
		return "", fmt.Errorf("authorityprocess: absolute edit path rejected: %q", rel)
	}
	root, err := t.canonicalRoot()
	if err != nil {
		return "", err
	}
	joined := filepath.Join(root, filepath.FromSlash(rel))
	within, err := filepath.Rel(root, joined)
	if err != nil || within == ".." || strings.HasPrefix(within, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("authorityprocess: edit path escapes the repository: %q", rel)
	}
	return joined, nil
}

// factoryToolchainEnv is the child's constructed environment. It mirrors the
// verification gate's: offline, non-interactive, and carrying only what a Go
// build genuinely needs. A gate that can reach the network is a gate that can
// exfiltrate the source it was handed.
func factoryToolchainEnv() []string {
	env := []string{
		"PATH=" + os.Getenv("PATH"),
		"LANG=C",
		"LC_ALL=C",
		"TZ=UTC",
		"GOFLAGS=-mod=mod",
		"GOPROXY=off",
		"GOTOOLCHAIN=local",
		"GIT_TERMINAL_PROMPT=0",
	}
	for _, key := range []string{"GOCACHE", "GOMODCACHE", "GOPATH", "GOROOT", "HOME", "TMPDIR"} {
		if value := os.Getenv(key); value != "" {
			env = append(env, key+"="+value)
		}
	}
	return env
}

// truncateToolchainOutput bounds a compiler's output so one failing gate
// cannot fill a log. The first lines carry the first error, which is the one
// that matters.
func truncateToolchainOutput(out []byte) string {
	const limit = 4 << 10
	text := strings.TrimSpace(string(out))
	if len(text) <= limit {
		return text
	}
	return text[:limit] + "... (truncated)"
}
