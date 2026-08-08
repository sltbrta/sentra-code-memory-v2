// This file contains the canonical SQL units shared by Reserve, Finalize, and Execute.
// Keeping transaction primitives together makes atomicity reviewable without
// exposing a second public persistence surface.
package localstate

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/audit"
	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/deletion"
	"github.com/sltbrta/sentra-code-memory-v2/services/internal/contracts"
)

func lookupCanonicalCommand(
	ctx context.Context,
	tx *sql.Tx,
	commandID string,
) (contracts.CommandRecord, bool, error) {
	command := canonicalCommandRecord(commandID)
	err := tx.QueryRowContext(ctx, `SELECT tenant_id,principal_id,session_id,command_type,
		idempotency_key,authenticated_digest,fence FROM commands WHERE command_id=?`, commandID).Scan(
		&command.Tenant.Value,
		&command.Principal.Value,
		&command.Session.Value,
		&command.CommandType,
		&command.IdempotencyKey,
		&command.AuthenticatedDigest.Hex,
		&command.Fence,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return contracts.CommandRecord{}, false, nil
	}
	if err != nil {
		return contracts.CommandRecord{}, false, fmt.Errorf("localstate: read canonical command: %w", err)
	}
	return command, true, nil
}

func lookupReservation(
	ctx context.Context,
	tx *sql.Tx,
	incoming contracts.CommandRecord,
) (Reservation, bool, error) {
	var commandID string
	var reservation Reservation
	err := tx.QueryRowContext(ctx, `SELECT c.command_id,c.tenant_id,c.principal_id,c.session_id,
		c.command_type,c.idempotency_key,c.authenticated_digest,c.fence,c.status
		FROM command_idempotency i JOIN commands c
		ON c.command_id=i.command_id AND c.tenant_id=i.tenant_id AND c.principal_id=i.principal_id
		AND c.session_id=i.session_id AND c.command_type=i.command_type
		AND c.idempotency_key=i.idempotency_key AND c.authenticated_digest=i.authenticated_digest
		AND c.fence=i.fence
		WHERE i.tenant_id=? AND i.principal_id=? AND i.command_type=? AND i.idempotency_key=?`,
		incoming.Tenant.Value,
		incoming.Principal.Value,
		incoming.CommandType,
		incoming.IdempotencyKey,
	).Scan(
		&commandID,
		&reservation.Command.Tenant.Value,
		&reservation.Command.Principal.Value,
		&reservation.Command.Session.Value,
		&reservation.Command.CommandType,
		&reservation.Command.IdempotencyKey,
		&reservation.Command.AuthenticatedDigest.Hex,
		&reservation.Command.Fence,
		&reservation.Status,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return Reservation{}, false, nil
	}
	if err != nil {
		return Reservation{}, false, fmt.Errorf("localstate: lookup command reservation: %w", err)
	}
	reservation.Command.Command = contracts.Identifier{Namespace: "command", Value: commandID}
	reservation.Command.Tenant.Namespace = "tenant"
	reservation.Command.Principal.Namespace = "principal"
	reservation.Command.Session.Namespace = "session"
	reservation.Command.AuthenticatedDigest.Algorithm = "sha256"
	if reservation.Command.AuthenticatedDigest.Hex != incoming.AuthenticatedDigest.Hex ||
		reservation.Command.Fence != incoming.Fence {
		return Reservation{}, false, ErrIdempotencyConflict
	}
	if reservation.Status == "completed" {
		receipt, err := lookupReceipt(ctx, tx, reservation.Command.Tenant.Value, commandID)
		if err != nil {
			return Reservation{}, false, err
		}
		reservation.Receipt = receipt
	}
	return reservation, true, nil
}

func canonicalCommandRecord(commandID string) contracts.CommandRecord {
	return contracts.CommandRecord{
		Command:             contracts.Identifier{Namespace: "command", Value: commandID},
		Tenant:              contracts.Identifier{Namespace: "tenant"},
		Principal:           contracts.Identifier{Namespace: "principal"},
		Session:             contracts.Identifier{Namespace: "session"},
		AuthenticatedDigest: contracts.Digest{Algorithm: "sha256"},
	}
}

func lookupReceipt(ctx context.Context, tx *sql.Tx, tenant, commandID string) (contracts.Receipt, error) {
	receipt := contracts.Receipt{OperationID: contracts.Identifier{Namespace: "command", Value: commandID}}
	if err := tx.QueryRowContext(ctx, `SELECT status,reason_code,causal_watermark FROM receipts
		WHERE tenant_id=? AND command_id=?`, tenant, commandID).
		Scan(&receipt.Status, &receipt.ReasonCode, &receipt.Watermark); err != nil {
		return contracts.Receipt{}, fmt.Errorf("localstate: read command receipt: %w", err)
	}
	return receipt, nil
}

func insertCommand(ctx context.Context, tx *sql.Tx, command contracts.CommandRecord, now int64) error {
	if _, err := tx.ExecContext(ctx, `INSERT INTO commands
		(command_id,tenant_id,principal_id,session_id,command_type,idempotency_key,authenticated_digest,fence,status,submitted_at_ms)
		VALUES (?,?,?,?,?,?,?,?, 'accepted',?)`, command.Command.Value, command.Tenant.Value, command.Principal.Value,
		command.Session.Value, command.CommandType, command.IdempotencyKey, command.AuthenticatedDigest.Hex, command.Fence, now); err != nil {
		return fmt.Errorf("localstate: insert command: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO command_idempotency
		(tenant_id,principal_id,session_id,command_type,idempotency_key,authenticated_digest,fence,command_id)
		VALUES (?,?,?,?,?,?,?,?)`, command.Tenant.Value, command.Principal.Value, command.Session.Value,
		command.CommandType, command.IdempotencyKey, command.AuthenticatedDigest.Hex, command.Fence, command.Command.Value); err != nil {
		return fmt.Errorf("localstate: insert idempotency: %w", err)
	}
	return nil
}

func completeMutation(
	ctx context.Context,
	tx *sql.Tx,
	mutation Mutation,
	clock contracts.Clock,
) (contracts.Receipt, error) {
	watermark, err := appendEvents(ctx, tx, mutation.Command, mutation.Events, clock.NowUnixMilli())
	if err != nil {
		return contracts.Receipt{}, err
	}
	receipt := mutation.Receipt
	receipt.OperationID = mutation.Command.Command
	receipt.Watermark = watermark
	if err := insertReceipt(ctx, tx, mutation.Command.Tenant.Value, receipt, clock.NowUnixMilli()); err != nil {
		return contracts.Receipt{}, err
	}
	if mutation.Projection != "" && watermark > 0 {
		if err := advanceWatermark(
			ctx,
			tx,
			mutation.Command.Tenant.Value,
			mutation.Projection,
			watermark,
			clock.NowUnixMilli(),
		); err != nil {
			return contracts.Receipt{}, err
		}
	}
	if mutation.Deletion != nil {
		request := *mutation.Deletion
		request.ReceiptID = receiptID(mutation.Command.Command.Value)
		if err := deletion.Tombstone(ctx, tx, request); err != nil {
			return contracts.Receipt{}, err
		}
		if mutation.PurgeNow {
			if err := deletion.CompletePurge(ctx, tx, request, clock.NowUnixMilli()); err != nil {
				return contracts.Receipt{}, err
			}
		}
	}
	result, err := tx.ExecContext(ctx, `UPDATE commands SET status='completed'
		WHERE tenant_id=? AND command_id=? AND status='accepted'`,
		mutation.Command.Tenant.Value,
		mutation.Command.Command.Value,
	)
	if err != nil {
		return contracts.Receipt{}, fmt.Errorf("localstate: complete command: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return contracts.Receipt{}, fmt.Errorf("localstate: read command completion: %w", err)
	}
	if rows != 1 {
		return contracts.Receipt{}, ErrReservationRequired
	}
	return receipt, nil
}

func appendEvents(ctx context.Context, tx *sql.Tx, command contracts.CommandRecord, events []MutationEvent, now int64) (uint64, error) {
	var watermark uint64
	for _, mutationEvent := range events {
		event := mutationEvent.Record
		var current uint64
		err := tx.QueryRowContext(ctx, `SELECT version FROM aggregate_versions
			WHERE tenant_id=? AND aggregate_type=? AND aggregate_id=?`, command.Tenant.Value,
			event.Aggregate.Namespace, event.Aggregate.Value).Scan(&current)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return 0, fmt.Errorf("localstate: read aggregate version: %w", err)
		}
		if event.Version != current+1 {
			return 0, ErrAggregateConflict
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO aggregate_versions(tenant_id,aggregate_type,aggregate_id,version)
			VALUES (?,?,?,?) ON CONFLICT(tenant_id,aggregate_type,aggregate_id) DO UPDATE SET version=excluded.version`,
			command.Tenant.Value, event.Aggregate.Namespace, event.Aggregate.Value, event.Version); err != nil {
			return 0, fmt.Errorf("localstate: advance aggregate: %w", err)
		}
		result, err := tx.ExecContext(ctx, `INSERT INTO events
			(event_id,tenant_id,aggregate_type,aggregate_id,aggregate_version,command_id,payload_digest,occurred_at_ms)
			VALUES (?,?,?,?,?,?,?,?)`, event.Event.Value, command.Tenant.Value, event.Aggregate.Namespace,
			event.Aggregate.Value, event.Version, command.Command.Value, event.PayloadDigest.Hex, now)
		if err != nil {
			return 0, fmt.Errorf("localstate: append event: %w", err)
		}
		sequence, err := result.LastInsertId()
		if err != nil || sequence <= 0 {
			return 0, fmt.Errorf("localstate: event sequence unavailable: %w", err)
		}
		watermark = uint64(sequence)
		if _, err := tx.ExecContext(ctx, `INSERT INTO outbox(outbox_id,tenant_id,event_sequence,payload_digest,delivered_at_ms)
			VALUES (?,?,?,?,NULL)`, "outbox:"+event.Event.Value, command.Tenant.Value, sequence, event.PayloadDigest.Hex); err != nil {
			return 0, fmt.Errorf("localstate: append outbox: %w", err)
		}
		metadata := audit.EventMetadata{
			Sequence: watermark, EventID: event.Event.Value, Tenant: command.Tenant.Value,
			AggregateType: event.Aggregate.Namespace, AggregateID: event.Aggregate.Value,
			AggregateVersion: event.Version, CommandID: command.Command.Value,
			PayloadDigest: event.PayloadDigest.Hex, OccurredAtMs: now,
		}
		if err := appendAudit(ctx, tx, metadata, now); err != nil {
			return 0, err
		}
	}
	return watermark, nil
}

func appendAudit(ctx context.Context, tx *sql.Tx, metadata audit.EventMetadata, now int64) error {
	var previous sql.NullString
	err := tx.QueryRowContext(ctx, `SELECT event_digest FROM audit_log WHERE tenant_id=? ORDER BY sequence DESC LIMIT 1`, metadata.Tenant).Scan(&previous)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("localstate: read audit head: %w", err)
	}
	digest, err := audit.Next(metadata, previous.String)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO audit_log(sequence,tenant_id,event_digest,previous_digest,recorded_at_ms)
		VALUES (?,?,?,?,?)`, metadata.Sequence, metadata.Tenant, digest, nullIfEmpty(previous.String), now); err != nil {
		return fmt.Errorf("localstate: append audit: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO checkpoints
		(checkpoint_id,tenant_id,event_sequence,audit_digest,key_epoch,created_at_ms) VALUES (?,?,?,?,0,?)
		ON CONFLICT(checkpoint_id) DO UPDATE SET tenant_id=excluded.tenant_id,event_sequence=excluded.event_sequence,
		audit_digest=excluded.audit_digest,created_at_ms=excluded.created_at_ms`, auditCheckpointID(metadata.Tenant),
		metadata.Tenant, metadata.Sequence, digest, now); err != nil {
		return fmt.Errorf("localstate: update audit checkpoint: %w", err)
	}
	return nil
}

func insertReceipt(ctx context.Context, tx *sql.Tx, tenant string, receipt contracts.Receipt, now int64) error {
	if _, err := tx.ExecContext(ctx, `INSERT INTO receipts
		(receipt_id,command_id,tenant_id,status,reason_code,causal_watermark,recorded_at_ms) VALUES (?,?,?,?,?,?,?)`,
		receiptID(receipt.OperationID.Value), receipt.OperationID.Value, tenant, receipt.Status, receipt.ReasonCode, receipt.Watermark, now); err != nil {
		return fmt.Errorf("localstate: insert receipt: %w", err)
	}
	return nil
}

func advanceWatermark(ctx context.Context, tx *sql.Tx, tenant, projection string, watermark uint64, now int64) error {
	if _, err := tx.ExecContext(ctx, `INSERT INTO watermarks(projection_name,tenant_id,watermark,generation,updated_at_ms)
		VALUES (?,?,?,1,?) ON CONFLICT(tenant_id,projection_name) DO UPDATE SET
		watermark=excluded.watermark,generation=watermarks.generation+1,updated_at_ms=excluded.updated_at_ms
		WHERE excluded.watermark>=watermarks.watermark`, projection, tenant, watermark, now); err != nil {
		return fmt.Errorf("localstate: advance watermark: %w", err)
	}
	return nil
}
