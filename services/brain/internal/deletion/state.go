// Package deletion applies immediate-deny tombstone and purge state transitions.
// Physical object deletion remains the ArtifactVault's responsibility.
package deletion

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

var ErrInvalidTransition = errors.New("deletion: invalid transition")

type executor interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

// Request identifies one already-authorized immutable artifact generation.
type Request struct {
	TenantID     string
	ArtifactID   string
	Generation   uint64
	ReceiptID    string
	TombstoneID  string
	PurgeID      string
	KeyEpoch     uint64
	ReasonCode   string
	OccurredAtMs int64
}

// Tombstone atomically changes published metadata to immediate-deny state and queues purge.
// The caller must run this inside the canonical command transaction after its receipt exists.
func Tombstone(ctx context.Context, tx executor, request Request) error {
	if tx == nil || !valid(request) {
		return ErrInvalidTransition
	}
	changed, err := tx.ExecContext(ctx, `UPDATE artifact_manifests SET status='tombstoned'
		WHERE tenant_id=? AND artifact_id=? AND generation=? AND status='published'`,
		request.TenantID, request.ArtifactID, request.Generation)
	if err != nil {
		return fmt.Errorf("deletion: tombstone manifest: %w", err)
	}
	if err := requireOne(changed); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO tombstones
		(tombstone_id,tenant_id,artifact_id,generation,receipt_id,reason_code,tombstoned_at_ms)
		VALUES (?,?,?,?,?,?,?)`, request.TombstoneID, request.TenantID, request.ArtifactID,
		request.Generation, request.ReceiptID, request.ReasonCode, request.OccurredAtMs); err != nil {
		return fmt.Errorf("deletion: insert tombstone: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO purge_jobs
		(purge_id,tenant_id,artifact_id,generation,tombstone_id,key_epoch,status,completed_at_ms)
		VALUES (?,?,?,?,?,?,'scheduled',NULL)`, request.PurgeID, request.TenantID,
		request.ArtifactID, request.Generation, request.TombstoneID, request.KeyEpoch); err != nil {
		return fmt.Errorf("deletion: schedule purge: %w", err)
	}
	return nil
}

// CompletePurge marks metadata purged only after the ArtifactVault reports disposition.
func CompletePurge(ctx context.Context, tx executor, request Request, completedAtMs int64) error {
	if tx == nil || !valid(request) || completedAtMs <= 0 {
		return ErrInvalidTransition
	}
	changed, err := tx.ExecContext(ctx, `UPDATE artifact_manifests SET status='purged'
		WHERE tenant_id=? AND artifact_id=? AND generation=? AND status='tombstoned'`,
		request.TenantID, request.ArtifactID, request.Generation)
	if err != nil {
		return fmt.Errorf("deletion: purge manifest: %w", err)
	}
	if err := requireOne(changed); err != nil {
		return err
	}
	changed, err = tx.ExecContext(ctx, `UPDATE purge_jobs SET status='completed',completed_at_ms=?
		WHERE tenant_id=? AND purge_id=? AND status='scheduled'`, completedAtMs, request.TenantID, request.PurgeID)
	if err != nil {
		return fmt.Errorf("deletion: complete purge: %w", err)
	}
	return requireOne(changed)
}

func valid(request Request) bool {
	return request.TenantID != "" && request.ArtifactID != "" && request.Generation > 0 &&
		request.ReceiptID != "" && request.TombstoneID != "" && request.PurgeID != "" &&
		request.ReasonCode != "" && request.OccurredAtMs > 0
}

func requireOne(result sql.Result) error {
	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("deletion: inspect transition: %w", err)
	}
	if rows != 1 {
		return ErrInvalidTransition
	}
	return nil
}
