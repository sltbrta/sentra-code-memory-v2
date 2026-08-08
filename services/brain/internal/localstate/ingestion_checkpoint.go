// This file hydrates the path-free Stage 03 restart checkpoint. Queries remain
// tenant/brain/source scoped and return static denial for absent domains rather
// than searching broader authority state.
package localstate

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

func loadCurrentCheckpoint(
	ctx context.Context,
	reader MetadataReader,
	scope IngestionScope,
) (IngestionCheckpoint, error) {
	var generationID string
	err := reader.QueryRowContext(ctx, `SELECT COALESCE(
		(SELECT generation_id FROM ingestion_current_generations
		 WHERE tenant_id=? AND brain_id=? AND source_id=?),
		(SELECT generation_id FROM ingestion_generations
		 WHERE tenant_id=? AND brain_id=? AND source_id=?
		 ORDER BY generation_sequence DESC LIMIT 1), '')`,
		scope.Tenant.Value, scope.Brain.Value, scope.SourceID,
		scope.Tenant.Value, scope.Brain.Value, scope.SourceID).Scan(&generationID)
	if errors.Is(err, sql.ErrNoRows) {
		return IngestionCheckpoint{}, ErrInvalidInput
	}
	if err != nil {
		return IngestionCheckpoint{}, fmt.Errorf("localstate: load current ingestion checkpoint: %w", err)
	}
	if generationID == "" {
		return IngestionCheckpoint{}, ErrInvalidInput
	}
	return loadGenerationCheckpoint(ctx, reader, scope, generationID)
}

func loadGenerationCheckpoint(
	ctx context.Context,
	reader MetadataReader,
	scope IngestionScope,
	generationID string,
) (IngestionCheckpoint, error) {
	checkpoint := IngestionCheckpoint{Scope: scope}
	var state string
	var revokedAt sql.NullInt64
	err := reader.QueryRowContext(ctx, `SELECT source.repository_id,source.configuration_digest,
		source.ignore_policy_digest,root.approved_root_id,source.acl_epoch,source.revocation_epoch,
		source.state,source.revoked_at_ms,generation.generation_id,generation.generation_sequence,
		generation.snapshot_id,snapshot.commit_oid,snapshot.tree_oid,snapshot.policy_digest,
		snapshot.snapshot_digest,generation.source_watermark,generation.state,
		COALESCE(previous.generation_id,''),COALESCE(previous_snapshot.commit_oid,''),
		EXISTS(SELECT 1 FROM ingestion_tombstones tombstone
		 WHERE tombstone.tenant_id=source.tenant_id AND tombstone.brain_id=source.brain_id
		 AND tombstone.source_id=source.source_id AND tombstone.target_kind='source')
		FROM ingestion_sources source
		JOIN ingestion_roots root USING (tenant_id,brain_id,source_id)
		JOIN ingestion_generations generation USING (tenant_id,brain_id,source_id)
		JOIN ingestion_snapshots snapshot
		 ON snapshot.tenant_id=generation.tenant_id AND snapshot.brain_id=generation.brain_id
		 AND snapshot.source_id=generation.source_id AND snapshot.snapshot_id=generation.snapshot_id
		LEFT JOIN ingestion_generations previous
		 ON previous.tenant_id=generation.tenant_id AND previous.brain_id=generation.brain_id
		 AND previous.source_id=generation.source_id
		 AND previous.generation_sequence=generation.generation_sequence-1
		LEFT JOIN ingestion_snapshots previous_snapshot
		 ON previous_snapshot.tenant_id=previous.tenant_id AND previous_snapshot.brain_id=previous.brain_id
		 AND previous_snapshot.source_id=previous.source_id AND previous_snapshot.snapshot_id=previous.snapshot_id
		WHERE source.tenant_id=? AND source.brain_id=? AND source.source_id=? AND generation.generation_id=?`,
		scope.Tenant.Value, scope.Brain.Value, scope.SourceID, generationID).Scan(
		&checkpoint.RepositoryID, &checkpoint.ConfigurationDigest, &checkpoint.IgnorePolicyDigest,
		&checkpoint.ApprovedRootID, &checkpoint.ACLEpoch, &checkpoint.RevocationEpoch,
		&state, &revokedAt, &checkpoint.GenerationID, &checkpoint.GenerationSequence,
		&checkpoint.SnapshotID, &checkpoint.CommitOID, &checkpoint.TreeOID, &checkpoint.PolicyDigest,
		&checkpoint.SnapshotDigest, &checkpoint.SourceWatermark, &checkpoint.GenerationState,
		&checkpoint.PreviousGenerationID, &checkpoint.PreviousCommitOID,
		&checkpoint.Tombstoned,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return IngestionCheckpoint{}, ErrInvalidInput
	}
	if err != nil {
		return IngestionCheckpoint{}, fmt.Errorf("localstate: load ingestion checkpoint: %w", err)
	}
	checkpoint.Revoked = state == "revoked"
	if checkpoint.Revoked != revokedAt.Valid {
		return IngestionCheckpoint{}, ErrIngestionConflict
	}
	if !validCheckpoint(checkpoint, state) {
		return IngestionCheckpoint{}, ErrIngestionConflict
	}
	return checkpoint, nil
}

func validCheckpoint(checkpoint IngestionCheckpoint, sourceState string) bool {
	if !validIngestionScope(checkpoint.Scope) || !validBoundedID(checkpoint.RepositoryID) ||
		!isSHA256(checkpoint.ConfigurationDigest) || !isSHA256(checkpoint.IgnorePolicyDigest) ||
		!isSHA256(checkpoint.ApprovedRootID) || checkpoint.GenerationSequence == 0 ||
		!validBoundedID(checkpoint.GenerationID) || !validBoundedID(checkpoint.SnapshotID) ||
		!isGitOID(checkpoint.CommitOID) || !isGitOID(checkpoint.TreeOID) ||
		!isSHA256(checkpoint.PolicyDigest) || !isSHA256(checkpoint.SnapshotDigest) ||
		(checkpoint.GenerationState != "ready" && checkpoint.GenerationState != "degraded") ||
		(sourceState != "ready" && sourceState != "revoked") {
		return false
	}
	if checkpoint.GenerationSequence == 1 {
		return checkpoint.PreviousGenerationID == "" && checkpoint.PreviousCommitOID == ""
	}
	return checkpoint.GenerationSequence == 2 && validBoundedID(checkpoint.PreviousGenerationID) &&
		isGitOID(checkpoint.PreviousCommitOID) && checkpoint.PreviousGenerationID != checkpoint.GenerationID &&
		checkpoint.PreviousCommitOID != checkpoint.CommitOID
}

func checkpointFromPublication(publication GenerationPublication) IngestionCheckpoint {
	return IngestionCheckpoint{
		Scope:               publication.Scope,
		RepositoryID:        publication.Source.RepositoryID,
		ConfigurationDigest: publication.Source.ConfigurationDigest,
		IgnorePolicyDigest:  publication.Source.IgnorePolicyDigest,
		ApprovedRootID:      publication.Source.ApprovedRootID,
		ACLEpoch:            publication.Source.ACLEpoch,
		GenerationID:        publication.GenerationID,
		GenerationSequence:  publication.Sequence,
		SnapshotID:          publication.Snapshot.SnapshotID,
		CommitOID:           publication.Snapshot.CommitOID,
		TreeOID:             publication.Snapshot.TreeOID,
		PolicyDigest:        publication.Snapshot.PolicyDigest,
		SnapshotDigest:      publication.Snapshot.SnapshotDigest,
		SourceWatermark:     publication.SourceWatermark,
		GenerationState:     publication.State,
	}
}
