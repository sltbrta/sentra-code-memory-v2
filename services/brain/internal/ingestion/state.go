package ingestion

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
)

const stateSchema = "ouroboros.stage03.ingestion-state.v3"

type operationKind string

const (
	operationAdmit     operationKind = "admit"
	operationReconcile operationKind = "reconcile"
	operationRevoke    operationKind = "revoke"
	operationTombstone operationKind = "tombstone"
)

type operationRecord struct {
	Kind      operationKind      `json:"kind"`
	Signature string             `json:"signature"`
	Result    *generationReceipt `json:"result,omitempty"`
	cached    *Generation
}

type generationReceipt struct {
	ID                 string `json:"id"`
	Sequence           uint64 `json:"sequence"`
	SnapshotID         string `json:"snapshot_id"`
	CommitOID          string `json:"commit_oid"`
	TreeOID            string `json:"tree_oid"`
	ManifestDigest     string `json:"manifest_digest"`
	PolicyDigest       string `json:"policy_digest"`
	DeltaDigest        string `json:"delta_digest"`
	ExpectedPreviousID string `json:"expected_previous_id,omitempty"`
	PreviousCommitOID  string `json:"previous_commit_oid,omitempty"`
}

type persistedState struct {
	Schema              string                      `json:"schema"`
	CheckpointDigest    string                      `json:"checkpoint_digest"`
	ConfigurationDigest string                      `json:"configuration_digest"`
	ApprovedRootID      string                      `json:"approved_root_id"`
	SourceID            string                      `json:"source_id"`
	Current             *generationReceipt          `json:"current,omitempty"`
	Operations          map[string]*operationRecord `json:"operations"`
	PendingHints        int                         `json:"pending_hints"`
	CoverageLost        bool                        `json:"coverage_lost"`
	Revoked             bool                        `json:"revoked"`
	Tombstoned          bool                        `json:"tombstoned"`
}

// MarshalBinary serializes restart metadata without plaintext repository paths
// or source bytes. The output is deterministic JSON suitable for an encrypted
// metadata authority. It returns ErrInvalidInput if internal invariants fail.
func (a *Authority) MarshalBinary() ([]byte, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	state := persistedState{
		Schema:              stateSchema,
		ConfigurationDigest: a.config.ConfigurationDigest,
		ApprovedRootID:      a.approvedRootID,
		SourceID:            a.sourceID,
		Operations:          make(map[string]*operationRecord, len(a.operations)),
		PendingHints:        a.pendingHints,
		CoverageLost:        a.coverageLost,
		Revoked:             a.revoked,
		Tombstoned:          a.tombstoned,
	}
	for key, record := range a.operations {
		state.Operations[key] = cloneOperationRecord(record)
	}
	if a.current != nil {
		receipt := receiptFromGeneration(*a.current, a.currentPreviousCommitOID)
		state.Current = &receipt
	}
	checkpointDigest, err := digestPersistedState(state)
	if err != nil {
		return nil, err
	}
	state.CheckpointDigest = checkpointDigest
	if err := validatePersistedState(state, a.config.MaxIdempotencyRecords, a.config.MaxFiles); err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(state)
	if err != nil {
		return nil, fmt.Errorf("marshal state: %w", err)
	}
	return encoded, nil
}

// Restore validates path-free restart metadata and reconstructs transient paths
// from exact committed trees. Active sources cost one scan for an initial
// generation and two for a delta generation; revoked sources restore deny state
// without hydrating Git bytes.
func Restore(ctx context.Context, config Config, encoded []byte) (*Authority, error) {
	if ctx == nil || len(encoded) == 0 {
		return nil, ErrInvalidInput
	}
	authority, err := New(ctx, config)
	if err != nil {
		return nil, err
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	var state persistedState
	if err := decoder.Decode(&state); err != nil {
		return nil, ErrInvalidInput
	}
	if err := requireJSONEOF(decoder); err != nil {
		return nil, err
	}
	if err := validatePersistedState(state, authority.config.MaxIdempotencyRecords, authority.config.MaxFiles); err != nil {
		return nil, err
	}
	if state.ConfigurationDigest != authority.config.ConfigurationDigest ||
		state.ApprovedRootID != authority.approvedRootID || state.SourceID != authority.sourceID {
		return nil, ErrInvalidInput
	}
	authority.operations = state.Operations
	for _, record := range authority.operations {
		if record.Kind == operationAdmit || record.Kind == operationReconcile {
			authority.normalOperations++
		}
	}
	authority.pendingHints = state.PendingHints
	authority.coverageLost = state.CoverageLost
	authority.revoked = state.Revoked
	authority.tombstoned = state.Tombstoned
	if state.Current == nil {
		return authority, nil
	}
	authority.currentPreviousCommitOID = state.Current.PreviousCommitOID
	if state.Revoked {
		authority.current = generationFromReceiptPathFree(authority.sourceID, *state.Current)
		return authority, nil
	}
	generation, err := authority.hydrateGeneration(ctx, *state.Current)
	if err != nil {
		return nil, err
	}
	authority.current = &generation
	return authority, nil
}

func validatePersistedState(state persistedState, maxOperations, maxHints int) error {
	if state.Schema != stateSchema || !isDigest(state.CheckpointDigest) ||
		!isDigest(state.ConfigurationDigest) ||
		!isDigest(state.ApprovedRootID) || !isDigest(state.SourceID) ||
		state.Operations == nil || state.PendingHints < 0 || state.PendingHints > maxHints ||
		len(state.Operations) > maxOperations+2 || state.Tombstoned && !state.Revoked {
		return ErrInvalidInput
	}
	if state.Current != nil && validateGenerationReceipt(*state.Current) != nil {
		return ErrInvalidInput
	}
	normalOperations := 0
	lifecycleOperations := make(map[operationKind]int, 2)
	currentOperationFound := false
	previousOperationFound := false
	for key, record := range state.Operations {
		if !isDigest(key) || record == nil || !isDigest(record.Signature) {
			return ErrInvalidInput
		}
		switch record.Kind {
		case operationAdmit, operationReconcile:
			normalOperations++
			if record.Result == nil || validateGenerationReceipt(*record.Result) != nil {
				return ErrInvalidInput
			}
			currentOperationFound = currentOperationFound ||
				state.Current != nil && *record.Result == *state.Current
			previousOperationFound = previousOperationFound || state.Current != nil &&
				state.Current.Sequence > 1 && record.Result.ID == state.Current.ExpectedPreviousID &&
				record.Result.Sequence == state.Current.Sequence-1 &&
				record.Result.CommitOID == state.Current.PreviousCommitOID
		case operationRevoke, operationTombstone:
			lifecycleOperations[record.Kind]++
			if lifecycleOperations[record.Kind] > 1 || record.Result != nil {
				return ErrInvalidInput
			}
		default:
			return ErrInvalidInput
		}
	}
	if normalOperations > maxOperations {
		return ErrInvalidInput
	}
	if state.Current == nil {
		if len(state.Operations) != 0 || state.Revoked || state.Tombstoned ||
			state.PendingHints != 0 || state.CoverageLost {
			return ErrInvalidInput
		}
	} else if !currentOperationFound || state.Revoked != (lifecycleOperations[operationRevoke] == 1) ||
		state.Tombstoned != (lifecycleOperations[operationTombstone] == 1) ||
		state.Current.Sequence > 1 && !previousOperationFound {
		return ErrInvalidInput
	}
	expectedDigest, err := digestPersistedState(state)
	if err != nil || state.CheckpointDigest != expectedDigest {
		return ErrInvalidInput
	}
	return nil
}

func digestPersistedState(state persistedState) (string, error) {
	state.CheckpointDigest = ""
	encoded, err := json.Marshal(state)
	if err != nil {
		return "", fmt.Errorf("marshal checkpoint digest: %w", err)
	}
	return identity("ouroboros.stage03.ingestion-checkpoint.v1", string(encoded)), nil
}

func validateGenerationReceipt(receipt generationReceipt) error {
	if !isDigest(receipt.ID) || receipt.Sequence == 0 || !isDigest(receipt.SnapshotID) ||
		!isGitOID(receipt.CommitOID) || !isGitOID(receipt.TreeOID) ||
		!isDigest(receipt.ManifestDigest) || !isDigest(receipt.PolicyDigest) ||
		!isDigest(receipt.DeltaDigest) {
		return ErrInvalidInput
	}
	if receipt.Sequence == 1 {
		if receipt.ExpectedPreviousID != "" || receipt.PreviousCommitOID != "" {
			return ErrInvalidInput
		}
		return nil
	}
	if !isDigest(receipt.ExpectedPreviousID) || !isGitOID(receipt.PreviousCommitOID) {
		return ErrInvalidInput
	}
	return nil
}

func receiptFromGeneration(generation Generation, previousCommitOID string) generationReceipt {
	return generationReceipt{
		ID: generation.ID, Sequence: generation.Sequence, SnapshotID: generation.SnapshotID,
		CommitOID: generation.CommitOID, TreeOID: generation.TreeOID,
		ManifestDigest: generation.Manifest.Digest, PolicyDigest: generation.Manifest.PolicyDigest,
		DeltaDigest:        digestChanges(generation.Delta),
		ExpectedPreviousID: generation.ExpectedPreviousID, PreviousCommitOID: previousCommitOID,
	}
}

func (a *Authority) hydrateGeneration(ctx context.Context, receipt generationReceipt) (Generation, error) {
	snapshot, err := a.readSnapshot(ctx, receipt.CommitOID)
	if err != nil {
		return Generation{}, err
	}
	var delta []Change
	if receipt.Sequence > 1 {
		previous, err := a.readSnapshot(ctx, receipt.PreviousCommitOID)
		if err != nil {
			return Generation{}, err
		}
		delta, err = deriveDelta(ctx, previous.manifest.Files, snapshot.manifest.Files)
		if err != nil {
			return Generation{}, err
		}
	}
	generation := generationFromSnapshot(a.sourceID, receipt.Sequence, receipt.ExpectedPreviousID, snapshot, delta)
	if !receiptMatchesGeneration(receipt, generation) {
		return Generation{}, ErrGit
	}
	return generation, nil
}

func receiptMatchesGeneration(receipt generationReceipt, generation Generation) bool {
	return receipt.ID == generation.ID && receipt.SnapshotID == generation.SnapshotID &&
		receipt.CommitOID == generation.CommitOID && receipt.TreeOID == generation.TreeOID &&
		receipt.ManifestDigest == generation.Manifest.Digest &&
		receipt.PolicyDigest == generation.Manifest.PolicyDigest &&
		receipt.DeltaDigest == digestChanges(generation.Delta)
}

func digestChanges(changes []Change) string {
	hasher := newIdentityHasher("ouroboros.stage03.generation-delta.v1")
	for _, change := range changes {
		writeIdentityField(hasher, string(change.Kind))
		writeIdentityField(hasher, change.OldPath)
		writeIdentityField(hasher, change.NewPath)
		writeIdentityField(hasher, change.OldID)
		writeIdentityField(hasher, change.NewID)
	}
	return finishIdentity(hasher)
}

func generationFromReceiptPathFree(sourceID string, receipt generationReceipt) *Generation {
	return &Generation{
		ID: receipt.ID, Sequence: receipt.Sequence, SourceID: sourceID,
		SnapshotID: receipt.SnapshotID, CommitOID: receipt.CommitOID, TreeOID: receipt.TreeOID,
		Manifest:           Manifest{Digest: receipt.ManifestDigest, PolicyDigest: receipt.PolicyDigest},
		ExpectedPreviousID: receipt.ExpectedPreviousID,
	}
}

func cloneOperationRecord(record *operationRecord) *operationRecord {
	clone := &operationRecord{Kind: record.Kind, Signature: record.Signature}
	if record.Result != nil {
		result := *record.Result
		clone.Result = &result
	}
	return clone
}

func requireJSONEOF(decoder *json.Decoder) error {
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); err != io.EOF {
		return ErrInvalidInput
	}
	return nil
}
