// This file persists complete Stage 03 ingestion generations and source
// revocation. Each public mutation owns one SQLite transaction containing the
// command ledger, canonical metadata, current pointer, and receipt.
package localstate

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"github.com/sltbrta/sentra-code-memory-v2/services/internal/contracts"
)

var p5Languages = [...]string{"go", "typescript", "python", "rust", "java"}

// PublishGeneration atomically publishes one complete path-free generation.
// It validates the authenticated session and monotonic current-pointer CAS
// before writing. Exact completed retries return the canonical receipt;
// conflicting reuse and stale writers make no changes.
func (s *Store) PublishGeneration(ctx context.Context, publication GenerationPublication) (IngestionExecution, error) {
	if s == nil || ctx == nil || !validPublication(publication) {
		return IngestionExecution{}, ErrInvalidInput
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db == nil {
		return IngestionExecution{}, ErrInvalidInput
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return IngestionExecution{}, fmt.Errorf("localstate: begin ingestion publication: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	replayed, receipt, err := prepareIngestionCommand(ctx, tx, publication.Command, publication.Scope)
	if err != nil {
		return IngestionExecution{}, err
	}
	if replayed {
		checkpoint, err := loadGenerationCheckpoint(ctx, tx, publication.Scope, publication.GenerationID)
		if err != nil {
			return IngestionExecution{}, err
		}
		return IngestionExecution{Receipt: receipt, Checkpoint: checkpoint, Replayed: true}, nil
	}
	if err := publishGenerationRows(ctx, tx, publication, s.clock.NowUnixMilli()); err != nil {
		return IngestionExecution{}, err
	}
	receipt = contracts.Receipt{
		OperationID: publication.Command.Command,
		Status:      "completed",
		ReasonCode:  "ingestion_generation_" + publication.State,
		Watermark:   publication.SourceWatermark,
	}
	if err := completeIngestionCommand(ctx, tx, publication.Command, receipt, s.clock.NowUnixMilli()); err != nil {
		return IngestionExecution{}, err
	}
	if err := tx.Commit(); err != nil {
		return IngestionExecution{}, fmt.Errorf("localstate: commit ingestion publication: %w", err)
	}
	checkpoint := checkpointFromPublication(publication)
	return IngestionExecution{Receipt: receipt, Checkpoint: checkpoint}, nil
}

// LoadIngestionCheckpoint returns the current active or most recently revoked
// source checkpoint after exact session/domain authentication. It never returns
// an absolute or repository-relative path and never falls back across domains.
func (s *Store) LoadIngestionCheckpoint(ctx context.Context, query IngestionCheckpointQuery) (IngestionCheckpoint, error) {
	if s == nil || ctx == nil || !validCheckpointQuery(query) {
		return IngestionCheckpoint{}, ErrInvalidInput
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db == nil {
		return IngestionCheckpoint{}, ErrInvalidInput
	}
	if err := verifyIngestionIdentity(ctx, s.db, query.Identity, query.Scope); err != nil {
		return IngestionCheckpoint{}, err
	}
	return loadCurrentCheckpoint(ctx, s.db, query.Scope)
}

// RevokeIngestionSource atomically revokes and tombstones one source and every
// active revision, removes the current pointer, and completes a receipt. Exact
// retries are safe; other attempts against a revoked source fail closed.
func (s *Store) RevokeIngestionSource(ctx context.Context, request IngestionRevocation) (IngestionExecution, error) {
	if s == nil || ctx == nil || !validRevocation(request) {
		return IngestionExecution{}, ErrInvalidInput
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db == nil {
		return IngestionExecution{}, ErrInvalidInput
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return IngestionExecution{}, fmt.Errorf("localstate: begin ingestion revocation: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	replayed, receipt, err := prepareIngestionCommand(ctx, tx, request.Command, request.Scope)
	if err != nil {
		return IngestionExecution{}, err
	}
	if replayed {
		checkpoint, err := loadCurrentCheckpoint(ctx, tx, request.Scope)
		if err != nil {
			return IngestionExecution{}, err
		}
		return IngestionExecution{Receipt: receipt, Checkpoint: checkpoint, Replayed: true}, nil
	}
	checkpoint, err := revokeIngestionRows(ctx, tx, request, s.clock.NowUnixMilli())
	if err != nil {
		return IngestionExecution{}, err
	}
	receipt = contracts.Receipt{
		OperationID: request.Command.Command,
		Status:      "completed",
		ReasonCode:  request.ReasonCode,
		Watermark:   checkpoint.SourceWatermark,
	}
	if err := completeIngestionCommand(ctx, tx, request.Command, receipt, s.clock.NowUnixMilli()); err != nil {
		return IngestionExecution{}, err
	}
	if err := tx.Commit(); err != nil {
		return IngestionExecution{}, fmt.Errorf("localstate: commit ingestion revocation: %w", err)
	}
	return IngestionExecution{Receipt: receipt, Checkpoint: checkpoint}, nil
}

func prepareIngestionCommand(
	ctx context.Context,
	tx *sql.Tx,
	command contracts.CommandRecord,
	scope IngestionScope,
) (bool, contracts.Receipt, error) {
	if err := verifyCommandIdentity(ctx, tx, command, scope); err != nil {
		return false, contracts.Receipt{}, err
	}
	canonical, commandFound, err := lookupCanonicalCommand(ctx, tx, command.Command.Value)
	if err != nil {
		return false, contracts.Receipt{}, err
	}
	if commandFound && canonical != command {
		return false, contracts.Receipt{}, ErrIngestionConflict
	}
	reservation, found, err := lookupReservation(ctx, tx, command)
	if errors.Is(err, ErrIdempotencyConflict) {
		return false, contracts.Receipt{}, ErrIngestionConflict
	}
	if err != nil {
		return false, contracts.Receipt{}, err
	}
	if found {
		if reservation.Command.AuthenticatedDigest != command.AuthenticatedDigest ||
			reservation.Command.Fence != command.Fence {
			return false, contracts.Receipt{}, ErrIngestionConflict
		}
		if reservation.Status != "completed" {
			return false, contracts.Receipt{}, ErrIngestionConflict
		}
		return true, reservation.Receipt, nil
	}
	if commandFound {
		return false, contracts.Receipt{}, ErrIngestionConflict
	}
	if err := insertCommand(ctx, tx, command, 0); err != nil {
		return false, contracts.Receipt{}, err
	}
	return false, contracts.Receipt{}, nil
}

func completeIngestionCommand(
	ctx context.Context,
	tx *sql.Tx,
	command contracts.CommandRecord,
	receipt contracts.Receipt,
	now int64,
) error {
	if _, err := tx.ExecContext(ctx, `UPDATE commands SET submitted_at_ms=? WHERE command_id=?`,
		now, command.Command.Value); err != nil {
		return fmt.Errorf("localstate: timestamp ingestion command: %w", err)
	}
	if err := insertReceipt(ctx, tx, command.Tenant.Value, receipt, now); err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, `UPDATE commands SET status='completed'
		WHERE tenant_id=? AND command_id=? AND status='accepted'`, command.Tenant.Value, command.Command.Value)
	if err != nil {
		return fmt.Errorf("localstate: complete ingestion command: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("localstate: read ingestion completion: %w", err)
	}
	if rows != 1 {
		return ErrIngestionConflict
	}
	return nil
}

func verifyCommandIdentity(ctx context.Context, reader MetadataReader, command contracts.CommandRecord, scope IngestionScope) error {
	if command.Tenant != scope.Tenant {
		return ErrIdentityMismatch
	}
	var principal, tenant string
	if err := reader.QueryRowContext(ctx, `SELECT principal_id,tenant_id FROM sessions WHERE session_id=?`,
		command.Session.Value).Scan(&principal, &tenant); err != nil {
		return ErrIdentityMismatch
	}
	if principal != command.Principal.Value || tenant != scope.Tenant.Value {
		return ErrIdentityMismatch
	}
	return nil
}

func verifyIngestionIdentity(
	ctx context.Context,
	reader MetadataReader,
	identity contracts.MappedIdentityFact,
	scope IngestionScope,
) error {
	if identity.Tenant != scope.Tenant {
		return ErrIdentityMismatch
	}
	var principal, tenant string
	var uid uint32
	if err := reader.QueryRowContext(ctx, `SELECT principal_id,tenant_id,peer_uid FROM sessions WHERE session_id=?`,
		identity.Session.Value).Scan(&principal, &tenant, &uid); err != nil {
		return ErrIdentityMismatch
	}
	if principal != identity.Principal.Value || tenant != scope.Tenant.Value || uid != identity.Credentials.UID {
		return ErrIdentityMismatch
	}
	return nil
}

func completeLanguageSet(readiness []IngestionReadiness) bool {
	seen := make(map[string]bool, len(p5Languages))
	for _, lane := range readiness {
		if !isP5Language(lane.Language) || seen[lane.Language] ||
			(lane.Coverage != "syntax_aware" && lane.Coverage != "lexical_degraded") ||
			len(lane.ReasonCode) > 128 {
			return false
		}
		if lane.Coverage == "syntax_aware" && lane.ReasonCode != "" {
			return false
		}
		if lane.Coverage == "lexical_degraded" && strings.TrimSpace(lane.ReasonCode) == "" {
			return false
		}
		seen[lane.Language] = true
	}
	return len(seen) == len(p5Languages)
}

func validPublication(publication GenerationPublication) bool {
	if !validIngestionCommand(publication.Command) || publication.Command.CommandType != IngestionPublishCommand ||
		!authenticatedDigestMatches(publication.Command.AuthenticatedDigest, GenerationPublicationDigest(publication)) ||
		!validIngestionScope(publication.Scope) || publication.Command.Tenant != publication.Scope.Tenant ||
		!validSourceMetadata(publication.Source) || !validSnapshot(publication.Snapshot) ||
		!validBoundedID(publication.GenerationID) || publication.Sequence == 0 ||
		(publication.State != "ready" && publication.State != "degraded") ||
		!completeLanguageSet(publication.Readiness) {
		return false
	}
	if publication.Sequence == 1 && publication.ExpectedCurrentGenerationID != "" ||
		publication.Sequence > 1 && !validBoundedID(publication.ExpectedCurrentGenerationID) ||
		publication.GenerationID == publication.ExpectedCurrentGenerationID {
		return false
	}
	degraded := false
	for _, lane := range publication.Readiness {
		degraded = degraded || lane.Coverage == "lexical_degraded"
	}
	if degraded != (publication.State == "degraded") || len(publication.Revisions) > 1_000_000 {
		return false
	}
	seenRevision := make(map[string]bool, len(publication.Revisions))
	seenObject := make(map[string]bool, len(publication.Revisions))
	seenPath := make(map[string]bool, len(publication.Revisions))
	for _, revision := range publication.Revisions {
		if !validRevision(revision) || seenRevision[revision.RevisionID] ||
			seenObject[revision.SourceObjectID] || seenPath[revision.PathDigest] {
			return false
		}
		seenRevision[revision.RevisionID] = true
		seenObject[revision.SourceObjectID] = true
		seenPath[revision.PathDigest] = true
	}
	return true
}

func validRevocation(request IngestionRevocation) bool {
	return validIngestionCommand(request.Command) && request.Command.CommandType == IngestionRevokeCommand &&
		authenticatedDigestMatches(request.Command.AuthenticatedDigest, IngestionRevocationDigest(request)) &&
		validIngestionScope(request.Scope) && request.Command.Tenant == request.Scope.Tenant &&
		validBoundedID(request.ExpectedCurrentGenerationID) && request.RevocationEpoch > 0 &&
		strings.TrimSpace(request.ReasonCode) != "" && len(request.ReasonCode) <= 128
}

func validIngestionCommand(command contracts.CommandRecord) bool {
	return validCommand(command) && validBoundedID(command.Command.Value) &&
		validBoundedID(command.Tenant.Value) && validBoundedID(command.Principal.Value) &&
		validBoundedID(command.Session.Value) && validBoundedID(command.IdempotencyKey) &&
		isSHA256(command.AuthenticatedDigest.Hex)
}

func validCheckpointQuery(query IngestionCheckpointQuery) bool {
	return validIngestionScope(query.Scope) && validID(query.Identity.Tenant, "tenant") &&
		validID(query.Identity.Principal, "principal") && validID(query.Identity.Session, "session")
}

func validIngestionScope(scope IngestionScope) bool {
	return validID(scope.Tenant, "tenant") && validID(scope.Brain, "brain") && validBoundedID(scope.SourceID)
}

func validSourceMetadata(source IngestionSourceMetadata) bool {
	return validBoundedID(source.RepositoryID) && isSHA256(source.ConfigurationDigest) &&
		isSHA256(source.IgnorePolicyDigest) && isSHA256(source.ApprovedRootID)
}

func validSnapshot(snapshot IngestionSnapshotMetadata) bool {
	return validBoundedID(snapshot.SnapshotID) && isGitOID(snapshot.CommitOID) && isGitOID(snapshot.TreeOID) &&
		isSHA256(snapshot.PolicyDigest) && isSHA256(snapshot.SnapshotDigest)
}

func validRevision(revision IngestionRevisionMetadata) bool {
	return validBoundedID(revision.RevisionID) && validBoundedID(revision.SourceObjectID) &&
		isSHA256(revision.PathDigest) && isGitOID(revision.GitBlobOID) &&
		isSHA256(revision.ContentDigest) && revision.ByteLength >= 0 &&
		(revision.EntryKind == "file" || revision.EntryKind == "symlink") &&
		strings.TrimSpace(revision.MediaType) != "" && len(revision.MediaType) <= 255 &&
		(revision.Language == "" || isP5Language(revision.Language)) &&
		(revision.PredecessorRevisionID == "" || validBoundedID(revision.PredecessorRevisionID)) &&
		revision.PredecessorRevisionID != revision.RevisionID
}

func validBoundedID(value string) bool {
	return value != "" && strings.TrimSpace(value) == value && len(value) <= 512
}

func isSHA256(value string) bool { return len(value) == 64 && isLowerHex(value) }

func isGitOID(value string) bool { return (len(value) == 40 || len(value) == 64) && isLowerHex(value) }

func isLowerHex(value string) bool {
	for _, character := range value {
		if (character < '0' || character > '9') && (character < 'a' || character > 'f') {
			return false
		}
	}
	return true
}

func isP5Language(language string) bool {
	for _, candidate := range p5Languages {
		if language == candidate {
			return true
		}
	}
	return false
}

func ingestionTombstoneID(scope IngestionScope, kind, target string, epoch uint64) string {
	digest := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%s\x00%s\x00%s\x00%s\x00%d",
		scope.Tenant.Value, scope.Brain.Value, scope.SourceID, kind, target, epoch)))
	return hex.EncodeToString(digest[:])
}

func ingestionGenerationTombstoneID(scope IngestionScope, target, generationID string) string {
	digest := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%s\x00%s\x00generation\x00%s\x00%s",
		scope.Tenant.Value, scope.Brain.Value, scope.SourceID, target, generationID)))
	return hex.EncodeToString(digest[:])
}
