// Package gitcandidate implements the Stage 05 exact-base atomic Git
// candidate store. A candidate is an isolated repository with its own object
// database, index, refs, config, and worktree, hydrated from the canonical
// approved root at one exact pinned base commit. Canonical objects are read
// through a read-only alternate; every write stays candidate-local.
//
// Application is all-or-nothing: edits are validated as a set, every mutation
// is re-authorized at mutation time, and any failure discards the complete
// isolated candidate directory, records a rollback receipt, and leaves the
// canonical worktree plus the complete .git inventory byte-identical.
// Idempotency records are process-local rebuildable projections; durable
// replay state belongs to the kernel ledger.
package gitcandidate

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/sltbrta/sentra-code-memory-v2/services/broker/internal/factory/changeset"
	"github.com/sltbrta/sentra-code-memory-v2/services/internal/contracts"
)

// Static non-disclosing failure vocabulary. Public edges collapse every
// denial to not_found_or_denied; these codes stay in internal trace only.
var (
	ErrInvalidInput = errors.New("gitcandidate: invalid input")
	ErrGit          = errors.New("gitcandidate: git operation failed")
	ErrBase         = errors.New("gitcandidate: base mismatch")
	ErrApply        = errors.New("gitcandidate: edit application failed")
	ErrDenied       = errors.New("gitcandidate: mutation denied")
	ErrConflict     = errors.New("gitcandidate: conflicting idempotency reuse")
	ErrLimit        = errors.New("gitcandidate: bounded limit exceeded")
)

// CandidateState is the bounded all-or-nothing candidate lifecycle used here.
type CandidateState string

const (
	// StateApplied means every edit landed in the isolated candidate.
	StateApplied CandidateState = "APPLIED"
	// StateRejected means the candidate was discarded with a rollback receipt.
	StateRejected CandidateState = "REJECTED"
)

// Rollback reason codes are a static internal trace vocabulary.
const (
	// ReasonApplyFailed records an edit-level application failure.
	ReasonApplyFailed = "candidate_apply_failed"
	// ReasonEscapeDenied records a sealed-runner escape attempt.
	ReasonEscapeDenied = "candidate_escape_denied"
	// ReasonRejected records a deterministic candidate rejection.
	ReasonRejected = "candidate_rejected"
)

// Config fixes the canonical approved root, the isolated candidate parent
// directory, the pinned Git executable, and bounded operation limits.
type Config struct {
	CanonicalRoot  string
	CandidateRoot  string
	GitExecutable  string
	CommandTimeout time.Duration
	MaxFiles       int
	MaxFileBytes   int64
	MaxTotalBytes  int64
}

const (
	maxConfiguredFiles              = 100_000
	maxConfiguredFileBytes    int64 = 16 << 20
	maxConfiguredTotalBytes   int64 = 64 << 20
	maxBufferedGitOutputBytes int64 = 128 << 20
)

// Store owns candidate construction and application for one canonical root.
// Applications serialize per store (applyMu), so every candidate tree is
// single-writer by construction.
type Store struct {
	config  Config
	applyMu sync.Mutex
	mu      sync.Mutex
	seen    map[string]recordedOutcome
}

// recordedOutcome is the process-local idempotency projection for one key.
// A zero outcome marks an in-flight application of inFlightCandidate.
type recordedOutcome struct {
	requestDigest     contracts.Digest
	outcome           Outcome
	inFlightCandidate string
}

// NewStore validates the configuration and returns a candidate store. The
// candidate root must never sit inside the canonical worktree, because
// candidate files would otherwise enter the canonical attestation.
func NewStore(config Config) (*Store, error) {
	if !filepath.IsAbs(config.CanonicalRoot) || !filepath.IsAbs(config.CandidateRoot) ||
		!filepath.IsAbs(config.GitExecutable) ||
		config.CommandTimeout <= 0 || config.CommandTimeout > 10*time.Minute ||
		config.MaxFiles <= 0 || config.MaxFiles > maxConfiguredFiles ||
		config.MaxFileBytes <= 0 || config.MaxFileBytes > maxConfiguredFileBytes ||
		config.MaxTotalBytes <= 0 || config.MaxTotalBytes > maxConfiguredTotalBytes ||
		config.MaxFileBytes > config.MaxTotalBytes {
		return nil, ErrInvalidInput
	}
	canonical, err := filepath.EvalSymlinks(config.CanonicalRoot)
	if err != nil {
		return nil, ErrInvalidInput
	}
	canonicalInfo, err := os.Lstat(canonical)
	if err != nil || !canonicalInfo.IsDir() {
		return nil, ErrInvalidInput
	}
	if _, err := os.Stat(filepath.Join(canonical, ".git")); err != nil {
		return nil, ErrInvalidInput
	}
	gitResolved, err := filepath.EvalSymlinks(config.GitExecutable)
	if err != nil {
		return nil, ErrInvalidInput
	}
	gitInfo, err := os.Stat(gitResolved)
	if err != nil || !gitInfo.Mode().IsRegular() || gitInfo.Mode()&0o111 == 0 {
		return nil, ErrInvalidInput
	}
	if err := os.MkdirAll(config.CandidateRoot, 0o700); err != nil {
		return nil, ErrInvalidInput
	}
	candidateRoot, err := filepath.EvalSymlinks(config.CandidateRoot)
	if err != nil {
		return nil, ErrInvalidInput
	}
	canonical = filepath.Clean(canonical)
	candidateRoot = filepath.Clean(candidateRoot)
	if withinRoot(canonical, candidateRoot) {
		return nil, ErrInvalidInput
	}
	config.CanonicalRoot = canonical
	config.CandidateRoot = candidateRoot
	config.GitExecutable = filepath.Clean(gitResolved)
	return &Store{config: config, seen: make(map[string]recordedOutcome)}, nil
}

// Candidate is one isolated exact-base repository. Path is the candidate
// root; every mutation stays beneath it.
type Candidate struct {
	ID         string
	Path       string
	BaseGitOID string
	store      *Store
}

// Begin constructs one isolated candidate at the exact pinned base commit.
// The base must resolve to a commit in the canonical repository; anything
// else fails closed before a candidate directory exists. The hydrated
// worktree must equal the base tree exactly, with no tracked drift.
func (s *Store) Begin(ctx context.Context, baseGitOID string) (*Candidate, error) {
	if ctx == nil || !isGitOID(baseGitOID) {
		return nil, ErrInvalidInput
	}
	resolved, err := s.runGit(ctx, s.config.CanonicalRoot, 256, "rev-parse", "--verify", baseGitOID+"^{commit}")
	if err != nil {
		return nil, ErrBase
	}
	if strings.TrimSpace(string(resolved)) != baseGitOID {
		return nil, ErrBase
	}
	gitDir, err := s.runGit(ctx, s.config.CanonicalRoot, 4096, "rev-parse", "--absolute-git-dir")
	if err != nil {
		return nil, ErrGit
	}
	random := make([]byte, 8)
	if _, err := rand.Read(random); err != nil {
		return nil, ErrGit
	}
	candidate := &Candidate{
		ID:         "candidate-" + hex.EncodeToString(random),
		Path:       filepath.Join(s.config.CandidateRoot, "candidate-"+hex.EncodeToString(random)),
		BaseGitOID: baseGitOID,
		store:      s,
	}
	if err := os.Mkdir(candidate.Path, 0o700); err != nil {
		return nil, ErrGit
	}
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.RemoveAll(candidate.Path)
		}
	}()
	if _, err := s.runGit(ctx, candidate.Path, 4096, "init", "-q", "-b", "candidate"); err != nil {
		return nil, err
	}
	alternates := filepath.Join(strings.TrimSpace(string(gitDir)), "objects")
	infoDir := filepath.Join(candidate.Path, ".git", "objects", "info")
	if err := os.MkdirAll(infoDir, 0o700); err != nil {
		return nil, ErrGit
	}
	if err := os.WriteFile(filepath.Join(infoDir, "alternates"), []byte(alternates+"\n"), 0o600); err != nil {
		return nil, ErrGit
	}
	if _, err := s.runGit(ctx, candidate.Path, 4096, "checkout", "-q", "--detach", baseGitOID); err != nil {
		return nil, err
	}
	head, err := s.runGit(ctx, candidate.Path, 256, "rev-parse", "HEAD")
	if err != nil || strings.TrimSpace(string(head)) != baseGitOID {
		return nil, ErrGit
	}
	status, err := s.runGit(ctx, candidate.Path, 4096, "status", "--porcelain=v1", "-z")
	if err != nil {
		return nil, err
	}
	if len(bytes.Trim(status, "\x00")) != 0 {
		return nil, ErrGit
	}
	cleanup = false
	return candidate, nil
}

// MutationAuthorizer reauthorizes every candidate mutation immediately before
// it executes. A denial discards the whole candidate. The Stage 05 effect
// broker implements this port; standalone callers supply an explicit one.
type MutationAuthorizer interface {
	AuthorizeMutation(ctx context.Context, mutation Mutation) error
}

// Mutation is one bounded candidate file mutation selected for authorization.
type Mutation struct {
	Index int
	Edit  changeset.Edit
}

// ApplyRequest binds one atomic edit set to its identities and authorizer.
// Tenant and Principal scope idempotency exactly as the canonical command
// record does: one principal's replay returns its original outcome, while a
// different principal under the same key is an independent scope and
// computes its own candidate.
type ApplyRequest struct {
	ChangeSetID    string
	IdempotencyKey string
	Tenant         contracts.Identifier
	Principal      contracts.Identifier
	Edits          []changeset.Edit
	Authorizer     MutationAuthorizer
}

// Outcome is the deterministic result of one application attempt. Exactly one
// of the post-image facts or the rollback receipt is present.
type Outcome struct {
	State            CandidateState
	CandidateID      string
	BaseGitOID       string
	ChangeSetDigest  contracts.Digest
	PostImageDigest  contracts.Digest
	PostImageTreeOID string
	Rollback         *RollbackReceipt
}

// RollbackReceipt records the deterministic discard of a rejected candidate.
type RollbackReceipt struct {
	Receipt         contracts.Receipt
	CandidateID     string
	BaseGitOID      string
	ChangeSetDigest contracts.Digest
	DiscardedEdits  int
	FailedEditIndex int
}

// Apply executes one exact-base atomic application. Edits are validated as a
// set, every mutation is authorized at mutation time, pre-image bytes must
// match the pinned before digests, and post-image bytes are re-verified
// against the after digests. Any failure — including a set-level validation
// rejection before the first mutation — discards the complete candidate
// directory and returns a rejected outcome carrying its rollback receipt
// alongside the typed cause. Idempotency is scoped by tenant, principal, and
// key: an exact replay returns the recorded outcome without re-executing and
// discards the fresh unused candidate, a conflicting reuse within one scope
// denies and also discards the fresh candidate, and a different principal
// under the same key is an independent scope that computes its own
// candidate. A concurrent same-scope application is rejected rather than
// serialized; no removal ever touches an in-flight candidate. Applications
// serialize per store, so candidate trees are single-writer by construction.
func (s *Store) Apply(ctx context.Context, candidate *Candidate, request ApplyRequest) (*Outcome, error) {
	if ctx == nil || candidate == nil || candidate.store != s ||
		request.ChangeSetID == "" || request.IdempotencyKey == "" || request.Authorizer == nil ||
		request.Tenant.Namespace != "tenant" || request.Tenant.Value == "" ||
		request.Principal.Namespace != "principal" || request.Principal.Value == "" ||
		len(request.Edits) == 0 || len(request.Edits) > changeset.MaxEdits {
		return nil, ErrInvalidInput
	}
	key := replayKey(request)
	requestDigest := digestApplyRequest(candidate.BaseGitOID, request)
	if err := changeset.Validate(request.Edits); err != nil {
		// Never remove a candidate an in-flight application owns: the record
		// check precedes any removal.
		if s.candidateInFlight(candidate.ID) {
			return nil, ErrConflict
		}
		rejected := &Outcome{
			State:       StateRejected,
			CandidateID: candidate.ID,
			BaseGitOID:  candidate.BaseGitOID,
			Rollback: candidate.rollback(
				ReasonRejected, ChangeSetDigest(candidate.BaseGitOID, request.Edits), 0, -1),
		}
		_ = os.RemoveAll(candidate.Path)
		return rejected, err
	}
	setDigest := ChangeSetDigest(candidate.BaseGitOID, request.Edits)
	s.mu.Lock()
	recorded, found := s.seen[key]
	if !found {
		s.seen[key] = recordedOutcome{
			requestDigest:     requestDigest,
			inFlightCandidate: candidate.ID,
		}
	}
	s.mu.Unlock()
	if found {
		// The replayed candidate is unused: discard it unless it is the very
		// candidate the recorded outcome describes or the candidate an
		// in-flight application is writing.
		if candidate.ID != recorded.outcome.CandidateID && candidate.ID != recorded.inFlightCandidate {
			_ = os.RemoveAll(candidate.Path)
		}
		if recorded.requestDigest != requestDigest || recorded.outcome.State == "" {
			return nil, ErrConflict
		}
		outcome := recorded.outcome
		return &outcome, nil
	}
	s.applyMu.Lock()
	executed, digest, treeOID, applyErr := s.applyAll(ctx, candidate, request)
	s.applyMu.Unlock()
	if applyErr != nil {
		rejected := &Outcome{
			State:           StateRejected,
			CandidateID:     candidate.ID,
			BaseGitOID:      candidate.BaseGitOID,
			ChangeSetDigest: setDigest,
			Rollback:        candidate.rollback(ReasonApplyFailed, setDigest, executed, executed),
		}
		s.mu.Lock()
		s.seen[key] = recordedOutcome{requestDigest: requestDigest, outcome: *rejected}
		s.mu.Unlock()
		return rejected, applyErr
	}
	result := &Outcome{
		State:            StateApplied,
		CandidateID:      candidate.ID,
		BaseGitOID:       candidate.BaseGitOID,
		ChangeSetDigest:  setDigest,
		PostImageDigest:  digest,
		PostImageTreeOID: treeOID,
	}
	s.mu.Lock()
	s.seen[key] = recordedOutcome{requestDigest: requestDigest, outcome: *result}
	s.mu.Unlock()
	return result, nil
}

// candidateInFlight reports whether any recorded application is currently
// writing the named candidate, under any idempotency scope.
func (s *Store) candidateInFlight(candidateID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, recorded := range s.seen {
		if recorded.outcome.State == "" && recorded.inFlightCandidate == candidateID {
			return true
		}
	}
	return false
}

// replayKey computes the idempotency scope: tenant, principal, and caller
// key, matching the canonical command-record idempotency scope.
func replayKey(request ApplyRequest) string {
	return request.Tenant.Value + "\x00" + request.Principal.Value + "\x00" + request.IdempotencyKey
}

// Discard rejects one candidate without an edit-level cause, removes the
// complete isolated directory, and returns its rollback receipt. It
// serializes with applications so it never removes a tree mid-mutation.
func (candidate *Candidate) Discard(reasonCode string, setDigest contracts.Digest) (*RollbackReceipt, error) {
	if candidate == nil || candidate.store == nil {
		return nil, ErrInvalidInput
	}
	receipt := candidate.rollback(reasonCode, setDigest, 0, -1)
	candidate.store.applyMu.Lock()
	err := os.RemoveAll(candidate.Path)
	candidate.store.applyMu.Unlock()
	if err != nil {
		return receipt, ErrGit
	}
	return receipt, nil
}

// ReadFile returns the current candidate bytes for one repository-relative
// path with the store byte bound. Symlink components deny, so reads cannot
// escape the candidate root. Callers must authorize the read separately.
func (candidate *Candidate) ReadFile(repositoryPath string) ([]byte, error) {
	if candidate == nil || candidate.store == nil {
		return nil, ErrInvalidInput
	}
	if err := changeset.ValidatePath(repositoryPath); err != nil {
		return nil, ErrInvalidInput
	}
	return readCandidateFile(candidate, repositoryPath, candidate.store.config.MaxFileBytes)
}

// applyAll authorizes and executes every mutation in declared order, then
// computes the post-image facts. On failure it removes the complete candidate
// directory and reports how many mutations executed before the failure.
func (s *Store) applyAll(ctx context.Context, candidate *Candidate, request ApplyRequest) (int, contracts.Digest, string, error) {
	executed := 0
	fail := func(err error) (int, contracts.Digest, string, error) {
		_ = os.RemoveAll(candidate.Path)
		return executed, contracts.Digest{}, "", err
	}
	for index, edit := range request.Edits {
		if err := ctx.Err(); err != nil {
			return fail(ErrApply)
		}
		if err := request.Authorizer.AuthorizeMutation(ctx, Mutation{Index: index, Edit: edit}); err != nil {
			return fail(fmt.Errorf("%w: %w", ErrDenied, err))
		}
		if err := s.applyOne(candidate, edit); err != nil {
			return fail(err)
		}
		executed++
	}
	for _, edit := range request.Edits {
		if edit.Op == changeset.OpDelete {
			continue
		}
		content, err := readCandidateFile(candidate, edit.Path, s.config.MaxFileBytes)
		if err != nil || changeset.DigestBytes(content) != edit.AfterDigest {
			return fail(ErrApply)
		}
	}
	if _, err := s.runGit(ctx, candidate.Path, 4096, "add", "-A"); err != nil {
		return fail(ErrGit)
	}
	treeOutput, err := s.runGit(ctx, candidate.Path, 256, "write-tree")
	if err != nil {
		return fail(ErrGit)
	}
	treeOID := strings.TrimSpace(string(treeOutput))
	if !isGitOID(treeOID) {
		return fail(ErrGit)
	}
	digest, err := s.PostImageDigest(candidate)
	if err != nil {
		return fail(err)
	}
	return executed, digest, treeOID, nil
}

// applyOne executes one validated mutation against the candidate worktree.
// Pre-image bytes must match the pinned before digest exactly, and no path
// component may be a symlink, so edits cannot escape the candidate root.
func (s *Store) applyOne(candidate *Candidate, edit changeset.Edit) error {
	switch edit.Op {
	case changeset.OpAdd:
		if _, err := lstatNoSymlink(candidate, edit.Path); err == nil {
			return ErrApply
		} else if !errors.Is(err, os.ErrNotExist) {
			return ErrApply
		}
		return writeCandidateFile(candidate, edit.Path, edit.NewContent, s.config.MaxFileBytes)
	case changeset.OpModify:
		if err := s.verifyPreimage(candidate, edit.Path, edit.BeforeDigest); err != nil {
			return err
		}
		return writeCandidateFile(candidate, edit.Path, edit.NewContent, s.config.MaxFileBytes)
	case changeset.OpDelete:
		if err := s.verifyPreimage(candidate, edit.Path, edit.BeforeDigest); err != nil {
			return err
		}
		if err := os.Remove(candidatePath(candidate, edit.Path)); err != nil {
			return ErrApply
		}
		return nil
	case changeset.OpRename:
		if err := s.verifyPreimage(candidate, edit.OldPath, edit.BeforeDigest); err != nil {
			return err
		}
		if _, err := lstatNoSymlink(candidate, edit.Path); err == nil {
			return ErrApply
		} else if !errors.Is(err, os.ErrNotExist) {
			return ErrApply
		}
		target := candidatePath(candidate, edit.Path)
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return ErrApply
		}
		if err := os.Rename(candidatePath(candidate, edit.OldPath), target); err != nil {
			return ErrApply
		}
		return writeCandidateFile(candidate, edit.Path, edit.NewContent, s.config.MaxFileBytes)
	}
	return ErrApply
}

// verifyPreimage reads one candidate file and requires the exact pinned
// before digest; missing, symlinked, oversized, or mismatched pre-images fail
// the edit.
func (s *Store) verifyPreimage(candidate *Candidate, editPath string, before contracts.Digest) error {
	content, err := readCandidateFile(candidate, editPath, s.config.MaxFileBytes)
	if err != nil {
		return ErrApply
	}
	if changeset.DigestBytes(content) != before {
		return ErrApply
	}
	return nil
}

// Attestation is the complete canonical repository inventory digest covering
// the worktree and every .git file, mode, and byte. Callers compare
// attestations before and after candidate operations; they must be equal.
type Attestation struct {
	Digest contracts.Digest
	Files  int
	Bytes  int64
}

// AttestCanonical computes the complete canonical worktree plus .git
// inventory attestation. It never writes to the canonical repository.
func (s *Store) AttestCanonical() (Attestation, error) {
	records, err := inventory(s.config.CanonicalRoot, "", s.config.MaxFiles, s.config.MaxTotalBytes)
	if err != nil {
		return Attestation{}, err
	}
	hasher := sha256.New()
	var total int64
	for _, record := range records {
		writeField(hasher, record.path)
		writeField(hasher, record.mode)
		writeField(hasher, strconv.FormatInt(record.size, 10))
		writeField(hasher, record.digest)
		total += record.size
	}
	return Attestation{
		Digest: contracts.Digest{Algorithm: "sha256", Hex: hex.EncodeToString(hasher.Sum(nil))},
		Files:  len(records),
		Bytes:  total,
	}, nil
}

// PostImageDigest recomputes the deterministic post-image digest of one
// candidate worktree, excluding its .git directory. The digest covers each
// file's path, mode, and content bytes, so a mode-only change is visible.
func (s *Store) PostImageDigest(candidate *Candidate) (contracts.Digest, error) {
	if candidate == nil || candidate.store != s {
		return contracts.Digest{}, ErrInvalidInput
	}
	records, err := inventory(candidate.Path, ".git", s.config.MaxFiles, s.config.MaxTotalBytes)
	if err != nil {
		return contracts.Digest{}, err
	}
	hasher := sha256.New()
	for _, record := range records {
		writeField(hasher, record.path)
		writeField(hasher, record.mode)
		writeField(hasher, record.digest)
	}
	return contracts.Digest{Algorithm: "sha256", Hex: hex.EncodeToString(hasher.Sum(nil))}, nil
}

func (candidate *Candidate) rollback(reasonCode string, setDigest contracts.Digest, discarded, failedIndex int) *RollbackReceipt {
	return &RollbackReceipt{
		Receipt: contracts.Receipt{
			OperationID: contracts.Identifier{Namespace: "candidate", Value: candidate.ID},
			Status:      "rejected",
			ReasonCode:  reasonCode,
		},
		CandidateID:     candidate.ID,
		BaseGitOID:      candidate.BaseGitOID,
		ChangeSetDigest: setDigest,
		DiscardedEdits:  discarded,
		FailedEditIndex: failedIndex,
	}
}

func candidatePath(candidate *Candidate, repositoryPath string) string {
	return filepath.Join(candidate.Path, filepath.FromSlash(repositoryPath))
}

// lstatNoSymlink resolves one repository-relative path component by component
// and rejects any symlink component, so a hostile pre-image cannot redirect
// reads or writes outside the candidate root.
func lstatNoSymlink(candidate *Candidate, repositoryPath string) (os.FileInfo, error) {
	current := candidate.Path
	segments := strings.Split(repositoryPath, "/")
	for index, segment := range segments {
		current = filepath.Join(current, segment)
		info, err := os.Lstat(current)
		if err != nil {
			return nil, err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return nil, ErrApply
		}
		if index < len(segments)-1 && !info.IsDir() {
			return nil, ErrApply
		}
	}
	return os.Lstat(current)
}

func readCandidateFile(candidate *Candidate, repositoryPath string, maxBytes int64) ([]byte, error) {
	info, err := lstatNoSymlink(candidate, repositoryPath)
	if err != nil || !info.Mode().IsRegular() {
		return nil, ErrApply
	}
	file, err := os.Open(candidatePath(candidate, repositoryPath))
	if err != nil {
		return nil, ErrApply
	}
	defer func() { _ = file.Close() }()
	content, err := io.ReadAll(io.LimitReader(file, maxBytes+1))
	if err != nil || int64(len(content)) > maxBytes {
		return nil, ErrApply
	}
	return content, nil
}

func writeCandidateFile(candidate *Candidate, repositoryPath string, content []byte, maxBytes int64) error {
	if int64(len(content)) > maxBytes {
		return ErrApply
	}
	segments := strings.Split(repositoryPath, "/")
	if len(segments) > 1 {
		if _, err := lstatNoSymlink(candidate, strings.Join(segments[:len(segments)-1], "/")); err != nil &&
			!errors.Is(err, os.ErrNotExist) {
			return ErrApply
		}
	}
	target := candidatePath(candidate, repositoryPath)
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return ErrApply
	}
	if err := os.WriteFile(target, content, 0o644); err != nil {
		return ErrApply
	}
	return nil
}

type inventoryRecord struct {
	path   string
	mode   string
	size   int64
	digest string
}

// inventory walks one root and returns sorted file records, skipping the
// named excluded top-level directory. Symlinks digest their target text.
func inventory(root, excludeTop string, maxFiles int, maxTotal int64) ([]inventoryRecord, error) {
	records := make([]inventoryRecord, 0, 256)
	var total int64
	err := filepath.WalkDir(root, func(current string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(root, current)
		if err != nil {
			return err
		}
		if relative == "." {
			return nil
		}
		slash := filepath.ToSlash(relative)
		if excludeTop != "" && (slash == excludeTop || strings.HasPrefix(slash, excludeTop+"/")) {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		record := inventoryRecord{path: slash, mode: info.Mode().String(), size: info.Size()}
		if info.Mode()&os.ModeSymlink != 0 {
			target, err := os.Readlink(current)
			if err != nil {
				return err
			}
			record.size = 0
			record.digest = "symlink:" + changeset.DigestBytes([]byte(target)).Hex
		} else if info.Mode().IsRegular() {
			if info.Size() > maxTotal-total {
				return ErrLimit
			}
			total += info.Size()
			content, err := os.ReadFile(current)
			if err != nil {
				return err
			}
			record.digest = changeset.DigestBytes(content).Hex
		} else {
			return ErrInvalidInput
		}
		records = append(records, record)
		if len(records) > maxFiles {
			return ErrLimit
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(records, func(i, j int) bool { return records[i].path < records[j].path })
	return records, nil
}

// runGit executes one bounded, scrubbed Git invocation against one directory.
// Hooks, fsmonitor, credentials, prompts, system config, maintenance, and
// optional locks are disabled; the environment carries no inherited
// authority, and dangerous GIT_* inheritance is absent by construction.
func (s *Store) runGit(ctx context.Context, directory string, maxOutput int64, args ...string) ([]byte, error) {
	if maxOutput > maxBufferedGitOutputBytes {
		return nil, ErrLimit
	}
	commandCtx, cancel := context.WithTimeout(ctx, s.config.CommandTimeout)
	defer cancel()
	baseArgs := []string{
		"--no-optional-locks",
		"--no-replace-objects",
		"-c", "core.hooksPath=/dev/null",
		"-c", "core.fsmonitor=false",
		"-c", "credential.helper=",
		"-c", "gc.auto=0",
		"-c", "maintenance.auto=false",
		"-C", directory,
	}
	cmd := exec.CommandContext(commandCtx, s.config.GitExecutable, append(baseArgs, args...)...)
	cmd.Env = []string{
		"HOME=/nonexistent",
		"LANG=C",
		"LC_ALL=C",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_OPTIONAL_LOCKS=0",
		"GIT_TERMINAL_PROMPT=0",
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, ErrGit
	}
	cmd.Stderr = io.Discard
	if err := cmd.Start(); err != nil {
		if commandCtx.Err() != nil {
			return nil, commandCtx.Err()
		}
		return nil, ErrGit
	}
	output, readErr := io.ReadAll(io.LimitReader(stdout, maxOutput+1))
	if int64(len(output)) > maxOutput {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return nil, ErrLimit
	}
	if readErr != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		if commandCtx.Err() != nil {
			return nil, commandCtx.Err()
		}
		return nil, ErrGit
	}
	if err := cmd.Wait(); err != nil {
		if commandCtx.Err() != nil {
			return nil, commandCtx.Err()
		}
		return nil, ErrGit
	}
	return output, nil
}

func digestApplyRequest(base string, request ApplyRequest) contracts.Digest {
	hasher := sha256.New()
	writeField(hasher, "ouroboros.stage05.apply-request.v1")
	writeField(hasher, base)
	writeField(hasher, request.Tenant.Value)
	writeField(hasher, request.Principal.Value)
	writeField(hasher, request.ChangeSetID)
	writeField(hasher, request.IdempotencyKey)
	for _, edit := range request.Edits {
		writeField(hasher, edit.Path)
		writeField(hasher, edit.OldPath)
		writeField(hasher, string(edit.Op))
		writeField(hasher, string(edit.Lang))
		writeField(hasher, edit.BeforeDigest.Hex)
		writeField(hasher, edit.AfterDigest.Hex)
	}
	return contracts.Digest{Algorithm: "sha256", Hex: hex.EncodeToString(hasher.Sum(nil))}
}

// ChangeSetDigest binds one edit set to its exact base. It is exported so the
// sealed runner can record rollback receipts for candidates it discards
// before an application attempt reaches the store.
func ChangeSetDigest(base string, edits []changeset.Edit) contracts.Digest {
	hasher := sha256.New()
	writeField(hasher, "ouroboros.stage05.change-set.v1")
	writeField(hasher, base)
	for _, edit := range edits {
		writeField(hasher, edit.Path)
		writeField(hasher, edit.OldPath)
		writeField(hasher, string(edit.Op))
		writeField(hasher, edit.BeforeDigest.Hex)
		writeField(hasher, edit.AfterDigest.Hex)
	}
	return contracts.Digest{Algorithm: "sha256", Hex: hex.EncodeToString(hasher.Sum(nil))}
}

func writeField(hasher io.Writer, value string) {
	_, _ = io.WriteString(hasher, strconv.Itoa(len(value)))
	_, _ = io.WriteString(hasher, ":")
	_, _ = io.WriteString(hasher, value)
	_, _ = io.WriteString(hasher, ";")
}

func withinRoot(root, candidate string) bool {
	relative, err := filepath.Rel(root, candidate)
	if err != nil {
		return false
	}
	return relative == "." || (relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)))
}

func isGitOID(value string) bool {
	if len(value) != 40 && len(value) != 64 {
		return false
	}
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded)*2 == len(value) && value == strings.ToLower(value) && utf8.ValidString(value)
}
