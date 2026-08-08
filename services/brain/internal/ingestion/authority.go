package ingestion

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
)

// Authority serializes lifecycle state for one approved committed Git root.
type Authority struct {
	mu                       sync.Mutex
	config                   Config
	approvedRootInfo         os.FileInfo
	approvedRootID           string
	sourceID                 string
	current                  *Generation
	currentPreviousCommitOID string
	operations               map[string]*operationRecord
	normalOperations         int
	pendingHints             int
	coverageLost             bool
	revoked                  bool
	tombstoned               bool
}

// New validates bootstrap authority and proves ApprovedRoot is the Git root.
func New(ctx context.Context, config Config) (*Authority, error) {
	if ctx == nil {
		return nil, ErrInvalidInput
	}
	validated, err := validateConfig(config)
	if err != nil {
		return nil, err
	}
	rootInfo, err := os.Lstat(validated.ApprovedRoot)
	if err != nil || !rootInfo.IsDir() || rootInfo.Mode()&os.ModeSymlink != 0 {
		return nil, ErrInvalidInput
	}
	authority := &Authority{
		config:           validated,
		approvedRootInfo: rootInfo,
		approvedRootID:   identity("ouroboros.stage03.approved-root.v1", validated.ConfigurationDigest),
		operations:       make(map[string]*operationRecord),
	}
	authority.sourceID = identity("ouroboros.stage03.source.v1", validated.TenantID,
		validated.BrainID, validated.RepositoryID, authority.approvedRootID)
	topLevel, err := authority.runGit(ctx, int64(validated.MaxPathBytes)+2, "rev-parse", "--path-format=absolute", "--show-toplevel")
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(string(topLevel)) != validated.ApprovedRoot {
		return nil, ErrOutOfRoot
	}
	return authority, nil
}

// Admit atomically publishes the initial committed generation.
func (a *Authority) Admit(ctx context.Context, request Admission) (Generation, error) {
	if err := validateOperationInput(ctx, request.IdempotencyKey); err != nil {
		return Generation{}, err
	}
	if !isGitOID(request.ExpectedCommitOID) {
		return Generation{}, ErrInvalidInput
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := a.denialError(); err != nil {
		return Generation{}, err
	}
	signature := identity("ouroboros.stage03.admit-request.v1", request.ExpectedCommitOID)
	record, exact, err := a.checkOperation(request.IdempotencyKey, signature)
	if err != nil {
		return Generation{}, err
	}
	if exact {
		return a.replayGenerationLocked(ctx, request.IdempotencyKey, signature, record)
	}
	if a.current != nil {
		if a.current.CommitOID == request.ExpectedCommitOID {
			if err := a.commitGenerationOperation(request.IdempotencyKey, signature, operationAdmit, *a.current, a.currentPreviousCommitOID); err != nil {
				return Generation{}, err
			}
			return cloneGeneration(*a.current), nil
		}
		return Generation{}, ErrConflict
	}
	var scanned snapshot
	scanErr := runUnlocked(&a.mu, func() error {
		var err error
		scanned, err = a.readSnapshot(ctx, request.ExpectedCommitOID)
		return err
	})
	if err := a.denialError(); err != nil {
		return Generation{}, err
	}
	if scanErr != nil {
		return Generation{}, scanErr
	}
	record, exact, err = a.checkOperation(request.IdempotencyKey, signature)
	if err != nil {
		return Generation{}, err
	}
	if exact {
		return a.replayGenerationLocked(ctx, request.IdempotencyKey, signature, record)
	}
	if a.current != nil {
		if a.current.CommitOID != request.ExpectedCommitOID {
			return Generation{}, ErrConflict
		}
		if err := a.commitGenerationOperation(request.IdempotencyKey, signature, operationAdmit, *a.current, a.currentPreviousCommitOID); err != nil {
			return Generation{}, err
		}
		return cloneGeneration(*a.current), nil
	}
	generation := generationFromSnapshot(a.sourceID, 1, "", scanned, nil)
	if err := a.commitGenerationOperation(request.IdempotencyKey, signature, operationAdmit, generation, ""); err != nil {
		return Generation{}, err
	}
	a.current = &generation
	a.currentPreviousCommitOID = ""
	return cloneGeneration(generation), nil
}

// Reconcile atomically replaces the current generation from a complete Git delta.
func (a *Authority) Reconcile(ctx context.Context, request ReconcileRequest) (Generation, error) {
	if err := validateOperationInput(ctx, request.IdempotencyKey); err != nil {
		return Generation{}, err
	}
	if !isDigest(request.ExpectedGenerationID) || !isGitOID(request.ExpectedCommitOID) ||
		!isGitOID(request.TargetCommitOID) {
		return Generation{}, ErrInvalidInput
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := a.denialError(); err != nil {
		return Generation{}, err
	}
	signature := identity(
		"ouroboros.stage03.reconcile-request.v1",
		request.ExpectedGenerationID,
		request.ExpectedCommitOID,
		request.TargetCommitOID,
	)
	record, exact, err := a.checkOperation(request.IdempotencyKey, signature)
	if err != nil {
		return Generation{}, err
	}
	if exact {
		return a.replayGenerationLocked(ctx, request.IdempotencyKey, signature, record)
	}
	if a.current == nil {
		return Generation{}, ErrStaleGeneration
	}
	if a.current.ID != request.ExpectedGenerationID || a.current.CommitOID != request.ExpectedCommitOID {
		return Generation{}, ErrStaleGeneration
	}
	if request.TargetCommitOID == request.ExpectedCommitOID {
		if err := a.commitGenerationOperation(request.IdempotencyKey, signature, operationReconcile, *a.current, a.currentPreviousCommitOID); err != nil {
			return Generation{}, err
		}
		a.pendingHints, a.coverageLost = 0, false
		return cloneGeneration(*a.current), nil
	}
	base := cloneGeneration(*a.current)
	var scanned snapshot
	var delta []Change
	scanErr := runUnlocked(&a.mu, func() error {
		var err error
		scanned, err = a.readSnapshot(ctx, request.TargetCommitOID)
		if err == nil {
			delta, err = deriveDelta(ctx, base.Manifest.Files, scanned.manifest.Files)
		}
		return err
	})
	if err := a.denialError(); err != nil {
		return Generation{}, err
	}
	if scanErr != nil {
		return Generation{}, scanErr
	}
	record, exact, err = a.checkOperation(request.IdempotencyKey, signature)
	if err != nil {
		return Generation{}, err
	}
	if exact {
		return a.replayGenerationLocked(ctx, request.IdempotencyKey, signature, record)
	}
	if a.current == nil || a.current.ID != base.ID || a.current.CommitOID != base.CommitOID {
		return Generation{}, ErrStaleGeneration
	}
	generation := generationFromSnapshot(a.sourceID, base.Sequence+1, base.ID, scanned, delta)
	if err := a.commitGenerationOperation(request.IdempotencyKey, signature, operationReconcile, generation, base.CommitOID); err != nil {
		return Generation{}, err
	}
	a.current = &generation
	a.currentPreviousCommitOID = base.CommitOID
	a.pendingHints, a.coverageLost = 0, false
	return cloneGeneration(generation), nil
}

// ObserveHints retains bounded watcher metadata without publishing revisions.
func (a *Authority) ObserveHints(hints []WatchHint) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.tombstoned {
		return ErrTombstoned
	}
	if a.revoked {
		return ErrRevoked
	}
	if len(hints) == 0 {
		return ErrInvalidInput
	}
	normalHints := 0
	overflow := false
	for _, hint := range hints {
		if hint.Kind != HintAdd && hint.Kind != HintModify && hint.Kind != HintRemove &&
			hint.Kind != HintRename && hint.Kind != HintOverflow {
			return ErrInvalidInput
		}
		if hint.Kind == HintOverflow {
			if hint.Path != "" || hint.OldPath != "" {
				return ErrInvalidInput
			}
			overflow = true
			continue
		}
		normalHints++
		if err := validateRepositoryPath(hint.Path, a.config.MaxPathBytes); err != nil {
			return err
		}
		if hint.Kind == HintRename {
			if err := validateRepositoryPath(hint.OldPath, a.config.MaxPathBytes); err != nil {
				return err
			}
		} else if hint.OldPath != "" {
			return ErrInvalidInput
		}
	}
	if normalHints > a.config.MaxFiles-a.pendingHints {
		return ErrLimit
	}
	a.pendingHints += normalHints
	a.coverageLost = a.coverageLost || overflow
	return nil
}

// Revoke applies immediate deny guarded by the current generation ID. Exact
// retries succeed; other retries return ErrRevoked.
func (a *Authority) Revoke(ctx context.Context, request RevokeRequest) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := validateOperationInput(ctx, request.IdempotencyKey); err != nil {
		return err
	}
	if !isDigest(request.ExpectedGenerationID) {
		return ErrInvalidInput
	}
	signature := identity("ouroboros.stage03.revoke-request.v1", request.ExpectedGenerationID)
	_, exact, err := a.checkOperation(request.IdempotencyKey, signature)
	if err != nil {
		return err
	}
	if exact {
		return nil
	}
	if a.tombstoned {
		return ErrTombstoned
	}
	if a.revoked {
		return ErrRevoked
	}
	if a.current == nil || a.current.ID != request.ExpectedGenerationID {
		return ErrStaleGeneration
	}
	if err := a.commitLifecycleOperation(request.IdempotencyKey, signature, operationRevoke); err != nil {
		return err
	}
	a.revoked = true
	a.pendingHints, a.coverageLost = 0, false
	return nil
}

// Tombstone removes path-bearing state after revoke. Exact retries succeed;
// other retries return ErrTombstoned.
func (a *Authority) Tombstone(ctx context.Context, request TombstoneRequest) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := validateOperationInput(ctx, request.IdempotencyKey); err != nil {
		return err
	}
	if !isDigest(request.ExpectedGenerationID) {
		return ErrInvalidInput
	}
	signature := identity("ouroboros.stage03.tombstone-request.v1", request.ExpectedGenerationID)
	_, exact, err := a.checkOperation(request.IdempotencyKey, signature)
	if err != nil {
		return err
	}
	if exact {
		return nil
	}
	if a.tombstoned {
		return ErrTombstoned
	}
	if !a.revoked || a.current == nil || a.current.ID != request.ExpectedGenerationID {
		return ErrStaleGeneration
	}
	if err := a.commitLifecycleOperation(request.IdempotencyKey, signature, operationTombstone); err != nil {
		return err
	}
	a.tombstoned = true
	a.current.Manifest.Files = nil
	a.current.Delta = nil
	return nil
}

// Current returns a defensive copy or denies revoked and tombstoned sources.
func (a *Authority) Current() (Generation, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.tombstoned {
		return Generation{}, ErrTombstoned
	}
	if a.revoked {
		return Generation{}, ErrRevoked
	}
	if a.current == nil {
		return Generation{}, ErrInvalidInput
	}
	return cloneGeneration(*a.current), nil
}

// Status returns opaque lifecycle and watcher metadata without path hydration.
func (a *Authority) Status() Status {
	a.mu.Lock()
	defer a.mu.Unlock()
	status := Status{
		SourceID:            a.sourceID,
		ApprovedRootID:      a.approvedRootID,
		PendingWatcherHints: a.pendingHints,
		WatcherCoverageLost: a.coverageLost,
		Revoked:             a.revoked,
		Tombstoned:          a.tombstoned,
	}
	if a.current != nil {
		status.CurrentGenerationID = a.current.ID
		status.CurrentCommitOID = a.current.CommitOID
		status.Sequence = a.current.Sequence
	}
	return status
}

// Rebuild rescans the current tree without mutation and returns ErrGit if its
// canonical projection differs from the admitted generation.
func (a *Authority) Rebuild(ctx context.Context) (Generation, error) {
	if ctx == nil {
		return Generation{}, ErrInvalidInput
	}
	if err := ctx.Err(); err != nil {
		return Generation{}, err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := a.denialError(); err != nil {
		return Generation{}, err
	}
	if a.current == nil {
		return Generation{}, ErrInvalidInput
	}
	base := cloneGeneration(*a.current)
	var scanned snapshot
	scanErr := runUnlocked(&a.mu, func() error {
		var err error
		scanned, err = a.readSnapshot(ctx, base.CommitOID)
		return err
	})
	if err := a.denialError(); err != nil {
		return Generation{}, err
	}
	if scanErr != nil {
		return Generation{}, scanErr
	}
	if a.current == nil || a.current.ID != base.ID || a.current.CommitOID != base.CommitOID {
		return Generation{}, ErrStaleGeneration
	}
	if scanned.tree != base.TreeOID || scanned.manifest.Digest != base.Manifest.Digest ||
		!equalFiles(scanned.manifest.Files, base.Manifest.Files) {
		return Generation{}, ErrGit
	}
	rebuilt := base
	rebuilt.Manifest = scanned.manifest
	return rebuilt, nil
}
func generationFromSnapshot(sourceID string, sequence uint64, previousID string, source snapshot, delta []Change) Generation {
	generationID := identity("ouroboros.stage03.generation.v1", sourceID,
		fmt.Sprintf("%d", sequence), previousID, source.manifest.Digest)
	return Generation{
		ID:                 generationID,
		Sequence:           sequence,
		SourceID:           sourceID,
		SnapshotID:         source.id,
		CommitOID:          source.commit,
		TreeOID:            source.tree,
		Manifest:           source.manifest,
		Delta:              append([]Change(nil), delta...),
		ExpectedPreviousID: previousID,
	}
}
func (a *Authority) checkOperation(key, signature string) (*operationRecord, bool, error) {
	keyDigest := digest(key)
	record, exists := a.operations[keyDigest]
	if !exists {
		return nil, false, nil
	}
	if record.Signature != signature {
		return nil, false, ErrConflict
	}
	return record, true, nil
}
func (a *Authority) commitGenerationOperation(key, signature string, kind operationKind, generation Generation, previousCommitOID string) error {
	keyDigest := digest(key)
	_, exists := a.operations[keyDigest]
	if !exists && a.normalOperations >= a.config.MaxIdempotencyRecords {
		return ErrLimit
	}
	receipt := receiptFromGeneration(generation, previousCommitOID)
	cached := cloneGeneration(generation)
	a.operations[keyDigest] = &operationRecord{Kind: kind, Signature: signature, Result: &receipt, cached: &cached}
	if !exists {
		a.normalOperations++
	}
	return nil
}
func (a *Authority) commitLifecycleOperation(key, signature string, kind operationKind) error {
	a.operations[digest(key)] = &operationRecord{Kind: kind, Signature: signature}
	return nil
}

// replayGenerationLocked rebuilds a path-free receipt and restores its lock.
func (a *Authority) replayGenerationLocked(ctx context.Context, key, signature string, record *operationRecord) (Generation, error) {
	if record.Result == nil {
		return Generation{}, ErrConflict
	}
	if record.cached != nil {
		return cloneGeneration(*record.cached), nil
	}
	receipt := *record.Result
	var generation Generation
	hydrateErr := runUnlocked(&a.mu, func() error {
		var err error
		generation, err = a.hydrateGeneration(ctx, receipt)
		return err
	})
	if err := a.denialError(); err != nil {
		return Generation{}, err
	}
	if hydrateErr != nil {
		return Generation{}, hydrateErr
	}
	currentRecord, exact, err := a.checkOperation(key, signature)
	if err != nil || !exact || currentRecord.Result == nil {
		if err != nil {
			return Generation{}, err
		}
		return Generation{}, ErrConflict
	}
	cached := cloneGeneration(generation)
	currentRecord.cached = &cached
	return cloneGeneration(generation), nil
}
func runUnlocked(mu *sync.Mutex, operation func() error) error {
	mu.Unlock()
	defer mu.Lock()
	return operation()
}
func (a *Authority) denialError() error {
	if a.tombstoned {
		return ErrTombstoned
	}
	if a.revoked {
		return ErrRevoked
	}
	return nil
}
func validateOperationInput(ctx context.Context, key string) error {
	if ctx == nil {
		return ErrInvalidInput
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if key == "" || len(key) > 512 || strings.TrimSpace(key) != key {
		return ErrInvalidInput
	}
	for _, character := range key {
		if character < 0x20 || character == 0x7f {
			return ErrInvalidInput
		}
	}
	return nil
}
