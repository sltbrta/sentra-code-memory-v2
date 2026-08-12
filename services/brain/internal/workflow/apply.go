package workflow

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	applyMaxEdits       = 128
	applyMaxFiles       = 25_000
	applyMaxCopyBytes   = int64(512 << 20)
	applyMaxFileBytes   = 8 << 20
	applyMaxCommands    = 8
	applyMaxCommandLen  = 4096
	applyOutputBytes    = 64 << 10
	applyMaxDuration    = 2 * time.Minute
	FailAfterStage      = "after_stage"
	FailDuringPromotion = "during_promotion"
)

// ApplyOptions controls bounded execution. InjectFailureAt exists only for
// deterministic crash-boundary tests and is intentionally not exposed by the
// JSONL/CLI/MCP request contract.
type ApplyOptions struct {
	InjectFailureAt string
}

// CommandReceipt records verification without returning command output.
type CommandReceipt struct {
	Command      string `json:"command"`
	Passed       bool   `json:"passed"`
	ExitCode     int    `json:"exit_code,omitempty"`
	OutputDigest string `json:"output_digest,omitempty"`
	Truncated    bool   `json:"truncated,omitempty"`
}

// ApplyReceipt is content-safe: it contains paths, digests and gate outcomes,
// never replacement text, staged source, or verifier output.
type ApplyReceipt struct {
	Schema       string           `json:"schema"`
	Applied      bool             `json:"applied"`
	Base         string           `json:"base,omitempty"`
	BeforeDigest string           `json:"before_digest,omitempty"`
	AfterDigest  string           `json:"after_digest,omitempty"`
	Paths        []string         `json:"paths,omitempty"`
	Validation   ValidationResult `json:"validation"`
	Verification []CommandReceipt `json:"verification,omitempty"`
	RolledBack   bool             `json:"rolled_back,omitempty"`
	Failure      string           `json:"failure,omitempty"`
	Digest       string           `json:"digest"`
}

// ApplyChangeSet freezes the requested base, builds and verifies a complete
// candidate in an isolated directory, then promotes all changed files. Every
// pre-promotion failure leaves the tree untouched; promotion failures restore
// all original files before returning.
func ApplyChangeSet(ctx context.Context, root string, cs ChangeSet, opts ApplyOptions) (ApplyReceipt, error) {
	receipt := ApplyReceipt{Schema: "sentra-scm.apply-changeset/v1", Base: cs.Base}
	fail := func(err error) (ApplyReceipt, error) {
		receipt.Applied = false
		receipt.Failure = err.Error()
		receipt.Digest = applyReceiptDigest(receipt)
		return receipt, err
	}
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, applyMaxDuration)
	defer cancel()
	if opts.InjectFailureAt != "" && opts.InjectFailureAt != FailAfterStage && opts.InjectFailureAt != FailDuringPromotion {
		return fail(fmt.Errorf("unknown failure injection point"))
	}
	if len(cs.Edits) == 0 || len(cs.Edits) > applyMaxEdits {
		return fail(fmt.Errorf("changeset edit count must be 1..%d", applyMaxEdits))
	}
	if len(cs.VerificationCommands) > applyMaxCommands {
		return fail(fmt.Errorf("verification command count exceeds %d", applyMaxCommands))
	}
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return fail(err)
	}
	rootAbs, err = filepath.EvalSymlinks(rootAbs)
	if err != nil {
		return fail(err)
	}

	// Validate structure and declared evidence before reading candidate content.
	pre := cs
	pre.Edits = append([]CandidateEdit(nil), cs.Edits...)
	for i := range pre.Edits {
		pre.Edits[i].ObservedDigest = pre.Edits[i].PredictedDigest
	}
	receipt.Validation = pre.Validate()
	if !receipt.Validation.Accepted {
		return fail(errors.New("changeset validation rejected"))
	}

	paths := uniqueEditPaths(cs.Edits)
	receipt.Paths = paths
	original := make(map[string][]byte, len(paths))
	modes := make(map[string]os.FileMode, len(paths))
	actualDigests := make(map[string]string, len(paths))
	for _, rel := range paths {
		abs, err := safeExistingFile(rootAbs, rel)
		if err != nil {
			return fail(err)
		}
		info, err := os.Stat(abs)
		if err != nil {
			return fail(err)
		}
		if info.Size() > applyMaxFileBytes {
			return fail(fmt.Errorf("%s exceeds %d-byte edit limit", rel, applyMaxFileBytes))
		}
		raw, err := os.ReadFile(abs)
		if err != nil {
			return fail(err)
		}
		actual := Digest(raw)
		if cs.BaseDigests[rel] != actual {
			receipt.Validation.Accepted = false
			receipt.Validation.Rejected = append(receipt.Validation.Rejected, RejectStaleBase)
			receipt.Validation.Reasons = append(receipt.Validation.Reasons, fmt.Sprintf("%s: working tree digest moved since freeze", rel))
			receipt.Validation = finalizeValidation(receipt.Validation)
			return fail(fmt.Errorf("stale base for %s", rel))
		}
		original[rel], modes[rel], actualDigests[rel] = raw, info.Mode().Perm(), actual
	}
	receipt.BeforeDigest = pathDigest(paths, actualDigests)
	if err := verifyGitBase(ctx, rootAbs, cs.Base); err != nil {
		return fail(err)
	}

	stage, cleanup, err := stageTree(ctx, rootAbs, cs.Base)
	if err != nil {
		return fail(err)
	}
	defer cleanup()
	if opts.InjectFailureAt == FailAfterStage {
		return fail(errors.New("injected failure after staging"))
	}

	observed := cs
	observed.Edits = append([]CandidateEdit(nil), cs.Edits...)
	afterBodies := make(map[string][]byte, len(paths))
	afterDigests := make(map[string]string, len(paths))
	for _, rel := range paths {
		stagePath, err := safeExistingFile(stage, rel)
		if err != nil {
			return fail(err)
		}
		raw, err := os.ReadFile(stagePath)
		if err != nil {
			return fail(err)
		}
		if Digest(raw) != actualDigests[rel] {
			return fail(fmt.Errorf("staged base differs for %s", rel))
		}
		body, err := applyFileEdits(raw, editsForPath(cs.Edits, rel))
		if err != nil {
			return fail(fmt.Errorf("%s: %w", rel, err))
		}
		if err := os.WriteFile(stagePath, body, modes[rel]); err != nil {
			return fail(err)
		}
		d := Digest(body)
		afterBodies[rel], afterDigests[rel] = body, d
		for i := range observed.Edits {
			if observed.Edits[i].Path == rel {
				observed.Edits[i].ObservedDigest = d
			}
		}
	}
	receipt.Validation = observed.Validate()
	if !receipt.Validation.Accepted {
		return fail(errors.New("observed candidate validation rejected"))
	}

	for _, command := range dedupStrings(cs.VerificationCommands) {
		if len(command) == 0 || len(command) > applyMaxCommandLen {
			return fail(fmt.Errorf("invalid verification command length"))
		}
		vr := runVerification(ctx, stage, command)
		receipt.Verification = append(receipt.Verification, vr)
		if !vr.Passed {
			return fail(fmt.Errorf("verification failed: %s", command))
		}
	}
	receipt.AfterDigest = pathDigest(paths, afterDigests)

	promoted := make([]string, 0, len(paths))
	rollback := func() {
		for i := len(promoted) - 1; i >= 0; i-- {
			rel := promoted[i]
			_ = atomicWrite(filepath.Join(rootAbs, filepath.FromSlash(rel)), original[rel], modes[rel])
		}
		receipt.RolledBack = len(promoted) > 0
	}
	for i, rel := range paths {
		if err := atomicWrite(filepath.Join(rootAbs, filepath.FromSlash(rel)), afterBodies[rel], modes[rel]); err != nil {
			rollback()
			return fail(fmt.Errorf("promote %s: %w", rel, err))
		}
		promoted = append(promoted, rel)
		if opts.InjectFailureAt == FailDuringPromotion && i == 0 {
			rollback()
			return fail(errors.New("injected failure during promotion"))
		}
	}
	receipt.Applied = true
	receipt.Digest = applyReceiptDigest(receipt)
	return receipt, nil
}

func uniqueEditPaths(edits []CandidateEdit) []string {
	seen := map[string]struct{}{}
	var out []string
	for _, e := range edits {
		if _, ok := seen[e.Path]; !ok {
			seen[e.Path] = struct{}{}
			out = append(out, e.Path)
		}
	}
	sort.Strings(out)
	return out
}
func editsForPath(edits []CandidateEdit, path string) []CandidateEdit {
	var out []CandidateEdit
	for _, e := range edits {
		if e.Path == path {
			out = append(out, e)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Range.Start > out[j].Range.Start })
	return out
}
func applyFileEdits(body []byte, edits []CandidateEdit) ([]byte, error) {
	out := append([]byte(nil), body...)
	for _, e := range edits {
		if e.Range.Start < 0 || e.Range.End < e.Range.Start || e.Range.End > len(out) {
			return nil, fmt.Errorf("range [%d,%d) outside %d-byte file", e.Range.Start, e.Range.End, len(out))
		}
		out = append(out[:e.Range.Start], append([]byte(e.Replacement), out[e.Range.End:]...)...)
	}
	return out, nil
}
func safeExistingFile(root, rel string) (string, error) {
	if err := validateChangePath(rel); err != nil {
		return "", err
	}
	if filepath.ToSlash(filepath.Clean(filepath.FromSlash(rel))) != rel {
		return "", fmt.Errorf("non-canonical path rejected: %q", rel)
	}
	abs := filepath.Join(root, filepath.FromSlash(rel))
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", err
	}
	r, err := filepath.Rel(root, resolved)
	if err != nil || r == ".." || strings.HasPrefix(r, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path escape rejected: %q", rel)
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("non-regular edit target rejected: %q", rel)
	}
	return resolved, nil
}
func verifyGitBase(ctx context.Context, root, base string) error {
	cmd := exec.CommandContext(ctx, "git", "-C", root, "rev-parse", "--show-toplevel")
	top, err := cmd.Output()
	if err != nil {
		return nil
	}
	resolved, err := filepath.EvalSymlinks(strings.TrimSpace(string(top)))
	if err != nil || resolved != root {
		return fmt.Errorf("apply root must be git worktree root")
	}
	if strings.TrimSpace(base) == "" {
		return fmt.Errorf("git base required")
	}
	cmd = exec.CommandContext(ctx, "git", "-C", root, "rev-parse", "HEAD")
	head, err := cmd.Output()
	if err != nil {
		return err
	}
	if strings.TrimSpace(string(head)) != strings.TrimSpace(base) {
		return fmt.Errorf("stale git base")
	}
	cmd = exec.CommandContext(ctx, "git", "-C", root, "status", "--porcelain", "--untracked-files=normal")
	status, err := cmd.Output()
	if err != nil {
		return err
	}
	for _, line := range strings.Split(strings.TrimSpace(string(status)), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "?? .sentra/") {
			continue
		}
		return fmt.Errorf("git base is not a clean frozen tree")
	}
	return nil
}
func stageTree(ctx context.Context, root, base string) (string, func(), error) {
	parent := filepath.Dir(root)
	stage, err := os.MkdirTemp(parent, ".sentra-changeset-")
	if err != nil {
		return "", func() {}, err
	}
	cleanup := func() { _ = os.RemoveAll(stage) }
	probe := exec.CommandContext(ctx, "git", "-C", root, "rev-parse", "--show-toplevel")
	if _, err := probe.Output(); err == nil {
		_ = os.Remove(stage)
		cmd := exec.CommandContext(ctx, "git", "-C", root, "worktree", "add", "--detach", "--quiet", stage, base)
		if out, err := cmd.CombinedOutput(); err != nil {
			return "", cleanup, fmt.Errorf("stage git worktree: %w (%s)", err, strings.TrimSpace(string(out)))
		}
		gitCleanup := func() {
			_ = exec.Command("git", "-C", root, "worktree", "remove", "--force", stage).Run()
			_ = os.RemoveAll(stage)
		}
		if err := validateStageSymlinks(stage); err != nil {
			gitCleanup()
			return "", func() {}, err
		}
		return stage, gitCleanup, nil
	}
	if err := copyTree(ctx, root, stage); err != nil {
		cleanup()
		return "", func() {}, err
	}
	if err := validateStageSymlinks(stage); err != nil {
		cleanup()
		return "", func() {}, err
	}
	return stage, cleanup, nil
}

func validateStageSymlinks(root string) error {
	return filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink == 0 {
			return nil
		}
		resolved, err := filepath.EvalSymlinks(path)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, resolved)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return fmt.Errorf("staging symlink escapes isolated tree: %s", path)
		}
		return nil
	})
}

func copyTree(ctx context.Context, root, dst string) error {
	files := 0
	var total int64
	return filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		if rel == "." {
			return nil
		}
		if info.IsDir() && (info.Name() == ".git" || info.Name() == ".sentra") {
			return filepath.SkipDir
		}
		if info.IsDir() {
			return os.MkdirAll(filepath.Join(dst, rel), info.Mode().Perm())
		}
		files++
		total += info.Size()
		if files > applyMaxFiles || total > applyMaxCopyBytes {
			return fmt.Errorf("staging copy limit exceeded")
		}
		target := filepath.Join(dst, rel)
		if info.Mode()&os.ModeSymlink != 0 {
			link, e := os.Readlink(path)
			if e != nil {
				return e
			}
			resolved, e := filepath.EvalSymlinks(path)
			if e != nil {
				return e
			}
			inside, e := filepath.Rel(root, resolved)
			if e != nil || inside == ".." || strings.HasPrefix(inside, ".."+string(filepath.Separator)) {
				return fmt.Errorf("staging symlink escapes root: %s", rel)
			}
			if filepath.IsAbs(link) {
				link = filepath.Join(dst, inside)
			}
			return os.Symlink(link, target)
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		raw, e := os.ReadFile(path)
		if e != nil {
			return e
		}
		return os.WriteFile(target, raw, info.Mode().Perm())
	})
}

type cappedBuffer struct {
	bytes.Buffer
	limit     int
	truncated bool
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	n := len(p)
	remain := b.limit - b.Len()
	if remain > 0 {
		if len(p) > remain {
			_, _ = b.Buffer.Write(p[:remain])
		} else {
			_, _ = b.Buffer.Write(p)
		}
	}
	if n > remain {
		b.truncated = true
	}
	return n, nil
}
func runVerification(ctx context.Context, dir, command string) CommandReceipt {
	buf := &cappedBuffer{limit: applyOutputBytes}
	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", command)
	cmd.Dir = dir
	cmd.Stdout = buf
	cmd.Stderr = buf
	err := cmd.Run()
	exit := 0
	if err != nil {
		exit = -1
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			exit = ee.ExitCode()
		}
	}
	sum := sha256.Sum256(buf.Bytes())
	return CommandReceipt{Command: command, Passed: err == nil, ExitCode: exit, OutputDigest: "sha256:" + hex.EncodeToString(sum[:16]), Truncated: buf.truncated}
}
func atomicWrite(path string, body []byte, mode os.FileMode) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".sentra-promote-")
	if err != nil {
		return err
	}
	name := f.Name()
	defer os.Remove(name)
	if err = f.Chmod(mode); err == nil {
		_, err = f.Write(body)
	}
	if err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return os.Rename(name, path)
}
func pathDigest(paths []string, digests map[string]string) string {
	h := sha256.New()
	for _, p := range paths {
		_, _ = io.WriteString(h, p+"\x00"+digests[p]+"\n")
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil)[:16])
}
func applyReceiptDigest(r ApplyReceipt) string {
	r.Digest = ""
	raw, _ := json.Marshal(r)
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:16])
}
