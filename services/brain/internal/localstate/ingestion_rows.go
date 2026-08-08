// This file contains the parameterized SQL units for Stage 03 ingestion state.
// Keeping row choreography private makes the Store API small and the atomic
// publication/revocation transaction straightforward to audit.
package localstate

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

func publishGenerationRows(ctx context.Context, tx *sql.Tx, publication GenerationPublication, now int64) error {
	if err := ensureSourceRows(ctx, tx, publication, now); err != nil {
		return err
	}
	if err := verifyGenerationCAS(ctx, tx, publication); err != nil {
		return err
	}
	if err := ensureSnapshot(ctx, tx, publication, now); err != nil {
		return err
	}
	for _, revision := range publication.Revisions {
		if err := ensureRevision(ctx, tx, publication, revision); err != nil {
			return err
		}
		if err := ensureSnapshotRevision(ctx, tx, publication, revision); err != nil {
			return err
		}
	}
	if err := tombstoneSupersededRevisions(ctx, tx, publication, now); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO ingestion_generations
		(tenant_id,brain_id,source_id,generation_id,generation_sequence,snapshot_id,state,source_watermark,created_at_ms)
		VALUES (?,?,?,?,?,?,'building',?,?)`, publication.Scope.Tenant.Value, publication.Scope.Brain.Value,
		publication.Scope.SourceID, publication.GenerationID, publication.Sequence,
		publication.Snapshot.SnapshotID, publication.SourceWatermark, now); err != nil {
		return fmt.Errorf("localstate: insert ingestion generation: %w", err)
	}
	for _, readiness := range publication.Readiness {
		if _, err := tx.ExecContext(ctx, `INSERT INTO ingestion_generation_readiness
			(tenant_id,brain_id,source_id,generation_id,language,coverage,reason_code)
			VALUES (?,?,?,?,?,?,?)`, publication.Scope.Tenant.Value, publication.Scope.Brain.Value,
			publication.Scope.SourceID, publication.GenerationID, readiness.Language,
			readiness.Coverage, readiness.ReasonCode); err != nil {
			return fmt.Errorf("localstate: insert ingestion readiness: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE ingestion_generations SET state=?,published_at_ms=?
		WHERE tenant_id=? AND brain_id=? AND source_id=? AND generation_id=? AND state='building'`,
		publication.State, now, publication.Scope.Tenant.Value, publication.Scope.Brain.Value,
		publication.Scope.SourceID, publication.GenerationID); err != nil {
		return fmt.Errorf("localstate: publish ingestion generation: %w", err)
	}
	if err := advanceCurrentGeneration(ctx, tx, publication, now); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE ingestion_sources SET state='ready',acl_epoch=?
		WHERE tenant_id=? AND brain_id=? AND source_id=? AND state<>'revoked'`, publication.Source.ACLEpoch,
		publication.Scope.Tenant.Value, publication.Scope.Brain.Value, publication.Scope.SourceID); err != nil {
		return fmt.Errorf("localstate: ready ingestion source: %w", err)
	}
	return nil
}

func ensureSnapshot(ctx context.Context, tx *sql.Tx, publication GenerationPublication, now int64) error {
	var snapshotID, commitOID, treeOID, policyDigest, snapshotDigest string
	var pathCount int
	err := tx.QueryRowContext(ctx, `SELECT snapshot_id,commit_oid,tree_oid,policy_digest,path_count,snapshot_digest
		FROM ingestion_snapshots WHERE tenant_id=? AND brain_id=? AND source_id=?
		AND (snapshot_id=? OR (commit_oid=? AND policy_digest=?))`,
		publication.Scope.Tenant.Value, publication.Scope.Brain.Value, publication.Scope.SourceID,
		publication.Snapshot.SnapshotID, publication.Snapshot.CommitOID, publication.Snapshot.PolicyDigest).Scan(
		&snapshotID, &commitOID, &treeOID, &policyDigest, &pathCount, &snapshotDigest,
	)
	if errors.Is(err, sql.ErrNoRows) {
		if _, err := tx.ExecContext(ctx, `INSERT INTO ingestion_snapshots
			(tenant_id,brain_id,source_id,snapshot_id,commit_oid,tree_oid,policy_digest,path_count,snapshot_digest,observed_at_ms)
			VALUES (?,?,?,?,?,?,?,?,?,?)`, publication.Scope.Tenant.Value, publication.Scope.Brain.Value,
			publication.Scope.SourceID, publication.Snapshot.SnapshotID, publication.Snapshot.CommitOID,
			publication.Snapshot.TreeOID, publication.Snapshot.PolicyDigest, len(publication.Revisions),
			publication.Snapshot.SnapshotDigest, now); err != nil {
			return fmt.Errorf("localstate: insert ingestion snapshot: %w", err)
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("localstate: read ingestion snapshot: %w", err)
	}
	if snapshotID != publication.Snapshot.SnapshotID || commitOID != publication.Snapshot.CommitOID ||
		treeOID != publication.Snapshot.TreeOID || policyDigest != publication.Snapshot.PolicyDigest ||
		pathCount != len(publication.Revisions) || snapshotDigest != publication.Snapshot.SnapshotDigest {
		return ErrIngestionConflict
	}
	return nil
}

func ensureSnapshotRevision(
	ctx context.Context,
	tx *sql.Tx,
	publication GenerationPublication,
	revision IngestionRevisionMetadata,
) error {
	var revisionID, sourceObjectID, pathDigest string
	err := tx.QueryRowContext(ctx, `SELECT source_revision_id,source_object_id,path_digest FROM ingestion_snapshot_revisions
		WHERE tenant_id=? AND brain_id=? AND source_id=? AND snapshot_id=?
		AND (source_revision_id=? OR source_object_id=? OR path_digest=?)`,
		publication.Scope.Tenant.Value, publication.Scope.Brain.Value, publication.Scope.SourceID,
		publication.Snapshot.SnapshotID, revision.RevisionID, revision.SourceObjectID, revision.PathDigest).Scan(
		&revisionID, &sourceObjectID, &pathDigest,
	)
	if errors.Is(err, sql.ErrNoRows) {
		if _, err := tx.ExecContext(ctx, `INSERT INTO ingestion_snapshot_revisions
			(tenant_id,brain_id,source_id,snapshot_id,source_revision_id,source_object_id,path_digest)
			VALUES (?,?,?,?,?,?,?)`, publication.Scope.Tenant.Value, publication.Scope.Brain.Value,
			publication.Scope.SourceID, publication.Snapshot.SnapshotID, revision.RevisionID,
			revision.SourceObjectID, revision.PathDigest); err != nil {
			return fmt.Errorf("localstate: insert ingestion membership: %w", err)
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("localstate: read ingestion membership: %w", err)
	}
	if revisionID != revision.RevisionID || sourceObjectID != revision.SourceObjectID || pathDigest != revision.PathDigest {
		return ErrIngestionConflict
	}
	return nil
}

func tombstoneSupersededRevisions(
	ctx context.Context,
	tx *sql.Tx,
	publication GenerationPublication,
	now int64,
) error {
	if publication.Sequence == 1 {
		return nil
	}
	var previousSnapshotID string
	var revocationEpoch uint64
	if err := tx.QueryRowContext(ctx, `SELECT generation.snapshot_id,source.revocation_epoch
		FROM ingestion_generations generation JOIN ingestion_sources source USING (tenant_id,brain_id,source_id)
		WHERE generation.tenant_id=? AND generation.brain_id=? AND generation.source_id=?
		AND generation.generation_id=?`, publication.Scope.Tenant.Value, publication.Scope.Brain.Value,
		publication.Scope.SourceID, publication.ExpectedCurrentGenerationID).Scan(
		&previousSnapshotID, &revocationEpoch,
	); err != nil {
		return fmt.Errorf("localstate: read previous ingestion membership: %w", err)
	}
	incoming := make(map[string]struct{}, len(publication.Revisions))
	for _, revision := range publication.Revisions {
		incoming[revision.RevisionID] = struct{}{}
	}
	rows, err := tx.QueryContext(ctx, `SELECT source_revision_id FROM ingestion_snapshot_revisions
		WHERE tenant_id=? AND brain_id=? AND source_id=? AND snapshot_id=? ORDER BY source_revision_id`,
		publication.Scope.Tenant.Value, publication.Scope.Brain.Value, publication.Scope.SourceID,
		previousSnapshotID)
	if err != nil {
		return fmt.Errorf("localstate: list previous ingestion membership: %w", err)
	}
	var removed []string
	for rows.Next() {
		var revisionID string
		if err := rows.Scan(&revisionID); err != nil {
			_ = rows.Close()
			return fmt.Errorf("localstate: scan previous ingestion membership: %w", err)
		}
		if _, unchanged := incoming[revisionID]; !unchanged {
			removed = append(removed, revisionID)
		}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return fmt.Errorf("localstate: iterate previous ingestion membership: %w", err)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("localstate: close previous ingestion membership: %w", err)
	}
	for _, revisionID := range removed {
		result, err := tx.ExecContext(ctx, `UPDATE ingestion_source_revisions SET deletion_state='tombstoned'
			WHERE tenant_id=? AND brain_id=? AND source_id=? AND source_revision_id=? AND deletion_state='active'`,
			publication.Scope.Tenant.Value, publication.Scope.Brain.Value, publication.Scope.SourceID, revisionID)
		if err != nil {
			return fmt.Errorf("localstate: tombstone superseded ingestion revision: %w", err)
		}
		changed, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("localstate: read superseded ingestion revision: %w", err)
		}
		if changed != 1 {
			return ErrIngestionConflict
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO ingestion_tombstones
			(tenant_id,brain_id,source_id,tombstone_id,target_kind,target_revision_id,revocation_epoch,reason_code,recorded_at_ms)
			VALUES (?,?,?,?,'source_revision',?,?,'generation_superseded',?)`,
			publication.Scope.Tenant.Value, publication.Scope.Brain.Value, publication.Scope.SourceID,
			ingestionGenerationTombstoneID(publication.Scope, revisionID, publication.GenerationID), revisionID,
			revocationEpoch, now); err != nil {
			return fmt.Errorf("localstate: record superseded ingestion revision: %w", err)
		}
	}
	return nil
}

func ensureSourceRows(ctx context.Context, tx *sql.Tx, publication GenerationPublication, now int64) error {
	var repositoryID, configurationDigest, ignorePolicyDigest, state string
	var aclEpoch uint64
	err := tx.QueryRowContext(ctx, `SELECT repository_id,configuration_digest,ignore_policy_digest,state,acl_epoch
		FROM ingestion_sources WHERE tenant_id=? AND brain_id=? AND source_id=?`, publication.Scope.Tenant.Value,
		publication.Scope.Brain.Value, publication.Scope.SourceID).Scan(
		&repositoryID, &configurationDigest, &ignorePolicyDigest, &state, &aclEpoch,
	)
	if errors.Is(err, sql.ErrNoRows) {
		if publication.Sequence != 1 {
			return ErrIngestionStale
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO ingestion_sources
			(tenant_id,brain_id,source_id,repository_id,configuration_digest,ignore_policy_digest,state,
			 acl_epoch,revocation_epoch,created_at_ms,revoked_at_ms)
			VALUES (?,?,?,?,?,?,'admitted',?,0,?,NULL)`, publication.Scope.Tenant.Value,
			publication.Scope.Brain.Value, publication.Scope.SourceID, publication.Source.RepositoryID,
			publication.Source.ConfigurationDigest, publication.Source.IgnorePolicyDigest,
			publication.Source.ACLEpoch, now); err != nil {
			return fmt.Errorf("localstate: insert ingestion source: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO ingestion_roots
			(tenant_id,brain_id,source_id,approved_root_id,symlink_policy) VALUES (?,?,?,?,'record_without_follow')`,
			publication.Scope.Tenant.Value, publication.Scope.Brain.Value, publication.Scope.SourceID,
			publication.Source.ApprovedRootID); err != nil {
			return fmt.Errorf("localstate: insert ingestion root: %w", err)
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("localstate: read ingestion source: %w", err)
	}
	if state == "revoked" {
		return ErrIngestionRevoked
	}
	if repositoryID != publication.Source.RepositoryID || configurationDigest != publication.Source.ConfigurationDigest ||
		ignorePolicyDigest != publication.Source.IgnorePolicyDigest || aclEpoch > publication.Source.ACLEpoch {
		return ErrIngestionConflict
	}
	var approvedRootID string
	if err := tx.QueryRowContext(ctx, `SELECT approved_root_id FROM ingestion_roots
		WHERE tenant_id=? AND brain_id=? AND source_id=?`, publication.Scope.Tenant.Value,
		publication.Scope.Brain.Value, publication.Scope.SourceID).Scan(&approvedRootID); err != nil {
		return fmt.Errorf("localstate: read ingestion root: %w", err)
	}
	if approvedRootID != publication.Source.ApprovedRootID {
		return ErrIngestionConflict
	}
	return nil
}

func verifyGenerationCAS(ctx context.Context, tx *sql.Tx, publication GenerationPublication) error {
	var currentID string
	err := tx.QueryRowContext(ctx, `SELECT generation_id FROM ingestion_current_generations
		WHERE tenant_id=? AND brain_id=? AND source_id=?`, publication.Scope.Tenant.Value,
		publication.Scope.Brain.Value, publication.Scope.SourceID).Scan(&currentID)
	if errors.Is(err, sql.ErrNoRows) {
		if publication.Sequence != 1 || publication.ExpectedCurrentGenerationID != "" {
			return ErrIngestionStale
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("localstate: read current ingestion generation: %w", err)
	}
	if publication.Sequence == 1 || currentID != publication.ExpectedCurrentGenerationID {
		return ErrIngestionStale
	}
	var currentSequence uint64
	if err := tx.QueryRowContext(ctx, `SELECT generation_sequence FROM ingestion_generations
		WHERE tenant_id=? AND brain_id=? AND source_id=? AND generation_id=?`, publication.Scope.Tenant.Value,
		publication.Scope.Brain.Value, publication.Scope.SourceID, currentID).Scan(&currentSequence); err != nil {
		return fmt.Errorf("localstate: read current ingestion sequence: %w", err)
	}
	if publication.Sequence != currentSequence+1 {
		return ErrIngestionStale
	}
	return nil
}

func ensureRevision(
	ctx context.Context,
	tx *sql.Tx,
	publication GenerationPublication,
	revision IngestionRevisionMetadata,
) error {
	var sourceObjectID, pathDigest, blobOID, contentDigest, entryKind, mediaType, deletionState string
	var language sql.NullString
	var byteLength int64
	var predecessor sql.NullString
	var aclEpoch uint64
	err := tx.QueryRowContext(ctx, `SELECT source_object_id,path_digest,git_blob_oid,content_digest,byte_length,
		entry_kind,media_type,language,predecessor_revision_id,deletion_state,acl_epoch FROM ingestion_source_revisions
		WHERE tenant_id=? AND brain_id=? AND source_id=? AND source_revision_id=?`, publication.Scope.Tenant.Value,
		publication.Scope.Brain.Value, publication.Scope.SourceID, revision.RevisionID).Scan(
		&sourceObjectID, &pathDigest, &blobOID, &contentDigest, &byteLength, &entryKind, &mediaType, &language,
		&predecessor, &deletionState, &aclEpoch,
	)
	if errors.Is(err, sql.ErrNoRows) {
		if revision.PredecessorRevisionID != "" {
			var predecessorObjectID string
			if err := tx.QueryRowContext(ctx, `SELECT source_object_id FROM ingestion_source_revisions
				WHERE tenant_id=? AND brain_id=? AND source_id=? AND source_revision_id=?`,
				publication.Scope.Tenant.Value, publication.Scope.Brain.Value, publication.Scope.SourceID,
				revision.PredecessorRevisionID).Scan(&predecessorObjectID); err != nil ||
				predecessorObjectID != revision.SourceObjectID {
				return ErrIngestionConflict
			}
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO ingestion_source_revisions
			(tenant_id,brain_id,source_id,source_revision_id,source_object_id,path_digest,git_blob_oid,
			 content_digest,byte_length,entry_kind,media_type,language,predecessor_revision_id,deletion_state,acl_epoch)
			VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,'active',?)`, publication.Scope.Tenant.Value,
			publication.Scope.Brain.Value, publication.Scope.SourceID, revision.RevisionID,
			revision.SourceObjectID, revision.PathDigest, revision.GitBlobOID, revision.ContentDigest,
			revision.ByteLength, revision.EntryKind, revision.MediaType, nullIfEmpty(revision.Language),
			nullIfEmpty(revision.PredecessorRevisionID),
			publication.Source.ACLEpoch); err != nil {
			return fmt.Errorf("localstate: insert ingestion revision: %w", err)
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("localstate: read ingestion revision: %w", err)
	}
	if sourceObjectID != revision.SourceObjectID || pathDigest != revision.PathDigest || blobOID != revision.GitBlobOID ||
		contentDigest != revision.ContentDigest || byteLength != revision.ByteLength || entryKind != revision.EntryKind ||
		mediaType != revision.MediaType || language.String != revision.Language ||
		predecessor.String != revision.PredecessorRevisionID || deletionState != "active" || aclEpoch != publication.Source.ACLEpoch {
		return ErrIngestionConflict
	}
	return nil
}

func advanceCurrentGeneration(ctx context.Context, tx *sql.Tx, publication GenerationPublication, now int64) error {
	if publication.Sequence == 1 {
		if _, err := tx.ExecContext(ctx, `INSERT INTO ingestion_current_generations
			(tenant_id,brain_id,source_id,generation_id,updated_at_ms) VALUES (?,?,?,?,?)`,
			publication.Scope.Tenant.Value, publication.Scope.Brain.Value, publication.Scope.SourceID,
			publication.GenerationID, now); err != nil {
			return fmt.Errorf("localstate: publish current ingestion generation: %w", err)
		}
		return nil
	}
	result, err := tx.ExecContext(ctx, `UPDATE ingestion_current_generations SET generation_id=?,updated_at_ms=?
		WHERE tenant_id=? AND brain_id=? AND source_id=? AND generation_id=?`, publication.GenerationID, now,
		publication.Scope.Tenant.Value, publication.Scope.Brain.Value, publication.Scope.SourceID,
		publication.ExpectedCurrentGenerationID)
	if err != nil {
		return fmt.Errorf("localstate: advance current ingestion generation: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("localstate: read ingestion CAS: %w", err)
	}
	if rows != 1 {
		return ErrIngestionStale
	}
	return nil
}

func revokeIngestionRows(
	ctx context.Context,
	tx *sql.Tx,
	request IngestionRevocation,
	now int64,
) (IngestionCheckpoint, error) {
	checkpoint, err := loadCurrentCheckpoint(ctx, tx, request.Scope)
	if err != nil {
		return IngestionCheckpoint{}, err
	}
	if checkpoint.Revoked {
		return IngestionCheckpoint{}, ErrIngestionRevoked
	}
	if checkpoint.GenerationID != request.ExpectedCurrentGenerationID ||
		request.RevocationEpoch <= checkpoint.RevocationEpoch {
		return IngestionCheckpoint{}, ErrIngestionStale
	}
	if _, err := tx.ExecContext(ctx, `UPDATE ingestion_sources SET state='revoked',revocation_epoch=?,revoked_at_ms=?
		WHERE tenant_id=? AND brain_id=? AND source_id=? AND state<>'revoked'`, request.RevocationEpoch, now,
		request.Scope.Tenant.Value, request.Scope.Brain.Value, request.Scope.SourceID); err != nil {
		return IngestionCheckpoint{}, fmt.Errorf("localstate: revoke ingestion source: %w", err)
	}
	rows, err := tx.QueryContext(ctx, `SELECT source_revision_id FROM ingestion_source_revisions
		WHERE tenant_id=? AND brain_id=? AND source_id=? AND deletion_state='active' ORDER BY source_revision_id`,
		request.Scope.Tenant.Value, request.Scope.Brain.Value, request.Scope.SourceID)
	if err != nil {
		return IngestionCheckpoint{}, fmt.Errorf("localstate: list active ingestion revisions: %w", err)
	}
	var revisionIDs []string
	for rows.Next() {
		var revisionID string
		if err := rows.Scan(&revisionID); err != nil {
			_ = rows.Close()
			return IngestionCheckpoint{}, fmt.Errorf("localstate: scan active ingestion revision: %w", err)
		}
		revisionIDs = append(revisionIDs, revisionID)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return IngestionCheckpoint{}, fmt.Errorf("localstate: iterate active ingestion revisions: %w", err)
	}
	if err := rows.Close(); err != nil {
		return IngestionCheckpoint{}, fmt.Errorf("localstate: close active ingestion revisions: %w", err)
	}
	for _, revisionID := range revisionIDs {
		if _, err := tx.ExecContext(ctx, `UPDATE ingestion_source_revisions SET deletion_state='tombstoned'
			WHERE tenant_id=? AND brain_id=? AND source_id=? AND source_revision_id=?`,
			request.Scope.Tenant.Value, request.Scope.Brain.Value, request.Scope.SourceID, revisionID); err != nil {
			return IngestionCheckpoint{}, fmt.Errorf("localstate: tombstone ingestion revision: %w", err)
		}
		if err := insertIngestionTombstone(ctx, tx, request, "source_revision", revisionID, now); err != nil {
			return IngestionCheckpoint{}, err
		}
	}
	if err := insertIngestionTombstone(ctx, tx, request, "source", "", now); err != nil {
		return IngestionCheckpoint{}, err
	}
	result, err := tx.ExecContext(ctx, `DELETE FROM ingestion_current_generations
		WHERE tenant_id=? AND brain_id=? AND source_id=? AND generation_id=?`, request.Scope.Tenant.Value,
		request.Scope.Brain.Value, request.Scope.SourceID, request.ExpectedCurrentGenerationID)
	if err != nil {
		return IngestionCheckpoint{}, fmt.Errorf("localstate: remove current ingestion generation: %w", err)
	}
	removed, err := result.RowsAffected()
	if err != nil {
		return IngestionCheckpoint{}, fmt.Errorf("localstate: read removed ingestion pointer: %w", err)
	}
	if removed != 1 {
		return IngestionCheckpoint{}, ErrIngestionStale
	}
	checkpoint.Revoked = true
	checkpoint.Tombstoned = true
	checkpoint.RevocationEpoch = request.RevocationEpoch
	return checkpoint, nil
}

func insertIngestionTombstone(
	ctx context.Context,
	tx *sql.Tx,
	request IngestionRevocation,
	kind, revisionID string,
	now int64,
) error {
	if _, err := tx.ExecContext(ctx, `INSERT INTO ingestion_tombstones
		(tenant_id,brain_id,source_id,tombstone_id,target_kind,target_revision_id,revocation_epoch,reason_code,recorded_at_ms)
		VALUES (?,?,?,?,?,?,?,?,?)`, request.Scope.Tenant.Value, request.Scope.Brain.Value, request.Scope.SourceID,
		ingestionTombstoneID(request.Scope, kind, revisionID, request.RevocationEpoch), kind,
		nullIfEmpty(revisionID), request.RevocationEpoch, request.ReasonCode, now); err != nil {
		return fmt.Errorf("localstate: insert ingestion tombstone: %w", err)
	}
	return nil
}
