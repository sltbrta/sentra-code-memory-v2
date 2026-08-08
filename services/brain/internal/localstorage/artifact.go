package localstorage

import (
	"context"
	"database/sql"
	"errors"
	"math"
	"strconv"
	"strings"

	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/artifactvault"
	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/localstate"
	"github.com/sltbrta/sentra-code-memory-v2/services/internal/contracts"
)

// ArtifactRepository persists ArtifactVault metadata and forward-only lifecycle
// state. It serializes reservations and publication, stores opaque locators and
// fences, and never persists payload, ciphertext, or key bytes.
type ArtifactRepository struct {
	authority *localstate.Store
}

type stageResult struct {
	record    artifactvault.GenerationRecord
	duplicate bool
}

// BeginStage reserves one exact next generation. An identical completed retry
// returns the canonical persisted record; changed or in-progress duplicates
// return artifactvault.ErrConflict and stale generations return ErrStaleGeneration.
func (r *ArtifactRepository) BeginStage(ctx context.Context, request contracts.ArtifactStageRequest, locator string) (artifactvault.GenerationRecord, bool, error) {
	if r == nil || r.authority == nil || !validManifest(request.Manifest) || request.ExpectedGeneration == math.MaxUint64 ||
		request.Manifest.Generation != request.ExpectedGeneration+1 || !validLocator(locator) {
		return artifactvault.GenerationRecord{}, false, artifactvault.ErrInvalid
	}
	result, err := writeResult(ctx, r.authority, func(writer localstate.MetadataWriter) (stageResult, error) {
		current, err := currentGeneration(ctx, writer, request.Manifest.Tenant.Value, request.Manifest.Artifact.Value)
		if err != nil {
			return stageResult{}, err
		}
		if current != request.ExpectedGeneration {
			return stageResult{}, artifactvault.ErrStaleGeneration
		}
		existing, err := loadArtifact(ctx, writer, request.Manifest.Tenant.Value, request.Manifest.Artifact.Value, request.Manifest.Generation)
		if err == nil {
			if !sameManifest(existing.Manifest, request.Manifest) {
				return stageResult{}, artifactvault.ErrConflict
			}
			if len(existing.Frames) == int(existing.Manifest.FrameCount) {
				return stageResult{record: existing, duplicate: true}, nil
			}
			return stageResult{}, artifactvault.ErrConflict
		}
		if !errors.Is(err, artifactvault.ErrNotFound) {
			return stageResult{}, err
		}
		fenceResult, err := writer.ExecContext(ctx, "INSERT INTO artifact_reservation_fences DEFAULT VALUES")
		if err != nil {
			return stageResult{}, operationError(ctx, "reserve artifact fence")
		}
		fence, err := fenceResult.LastInsertId()
		if err != nil || fence <= 0 {
			return stageResult{}, operationError(ctx, "read artifact fence")
		}
		manifest := request.Manifest
		_, err = writer.ExecContext(ctx, `INSERT INTO artifact_manifests
			(artifact_id,tenant_id,generation,content_digest,byte_length,frame_count,key_epoch,status)
			VALUES (?,?,?,?,?,?,?,'staged')`, manifest.Artifact.Value, manifest.Tenant.Value,
			manifest.Generation, manifest.Digest.Hex, manifest.Length, manifest.FrameCount, manifest.KeyEpoch)
		if err != nil {
			return stageResult{}, artifactvault.ErrConflict
		}
		_, err = writer.ExecContext(ctx, `INSERT INTO artifact_reservations
			(tenant_id,artifact_id,generation,locator,reservation_fence) VALUES (?,?,?,?,?)`,
			manifest.Tenant.Value, manifest.Artifact.Value, manifest.Generation, locator, fence)
		if err != nil {
			return stageResult{}, artifactvault.ErrConflict
		}
		return stageResult{record: artifactvault.GenerationRecord{
			Manifest: manifest, Locator: locator, Status: artifactvault.StatusStaged, Fence: uint64(fence),
		}}, nil
	})
	return result.record, result.duplicate, err
}

// CompleteStage atomically persists a complete contiguous frame manifest for the
// exact reservation fence. Exact retries succeed; partial or changed retries
// return ErrIncomplete or ErrConflict without changing canonical metadata.
func (r *ArtifactRepository) CompleteStage(ctx context.Context, record artifactvault.GenerationRecord) error {
	if r == nil || r.authority == nil || !validManifest(record.Manifest) {
		return artifactvault.ErrInvalid
	}
	if !validFrames(record.Manifest, record.Frames) {
		return artifactvault.ErrIncomplete
	}
	return writeOnly(ctx, r.authority, func(writer localstate.MetadataWriter) error {
		existing, err := loadArtifact(ctx, writer, record.Manifest.Tenant.Value, record.Manifest.Artifact.Value, record.Manifest.Generation)
		if err != nil {
			return artifactvault.ErrConflict
		}
		if existing.Fence != record.Fence || existing.Locator != record.Locator ||
			existing.Status != artifactvault.StatusStaged || !sameManifest(existing.Manifest, record.Manifest) {
			return artifactvault.ErrConflict
		}
		if len(existing.Frames) != 0 {
			if sameFrames(existing.Frames, record.Frames) {
				return nil
			}
			return artifactvault.ErrConflict
		}
		for _, frame := range record.Frames {
			if _, err := writer.ExecContext(ctx, `INSERT INTO artifact_frames
				(tenant_id,artifact_id,generation,frame_index,offset_bytes,length_bytes,frame_digest)
				VALUES (?,?,?,?,?,?,?)`,
				record.Manifest.Tenant.Value, record.Manifest.Artifact.Value, record.Manifest.Generation,
				frame.Index, frame.Offset, frame.Length, frame.ObjectDigest.Hex); err != nil {
				return artifactvault.ErrConflict
			}
		}
		return nil
	})
}

// AbortStage removes only an incomplete reservation matching the exact fence.
// Missing reservations are idempotent; completed or stale reservations conflict.
func (r *ArtifactRepository) AbortStage(ctx context.Context, record artifactvault.GenerationRecord) error {
	if r == nil || r.authority == nil || !validManifest(record.Manifest) || record.Fence == 0 {
		return artifactvault.ErrInvalid
	}
	return writeOnly(ctx, r.authority, func(writer localstate.MetadataWriter) error {
		existing, err := loadArtifact(ctx, writer, record.Manifest.Tenant.Value, record.Manifest.Artifact.Value, record.Manifest.Generation)
		if errors.Is(err, artifactvault.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		if existing.Fence != record.Fence || existing.Status != artifactvault.StatusStaged || len(existing.Frames) != 0 {
			return artifactvault.ErrConflict
		}
		if _, err := writer.ExecContext(ctx, `DELETE FROM artifact_manifests
			WHERE tenant_id=? AND artifact_id=? AND generation=?`,
			record.Manifest.Tenant.Value, record.Manifest.Artifact.Value, record.Manifest.Generation); err != nil {
			return operationError(ctx, "abort artifact stage")
		}
		return nil
	})
}

// Get returns one exact tenant/artifact/generation metadata record. Cross-tenant,
// absent, and malformed scopes fail closed without revealing another namespace.
func (r *ArtifactRepository) Get(ctx context.Context, tenant, artifact contracts.Identifier, generation uint64) (artifactvault.GenerationRecord, error) {
	if r == nil || r.authority == nil || !validID(tenant, "tenant") || !validID(artifact, "artifact") || generation == 0 {
		return artifactvault.GenerationRecord{}, artifactvault.ErrInvalid
	}
	return readResult(ctx, r.authority, func(reader queryer) (artifactvault.GenerationRecord, error) {
		return loadArtifact(ctx, reader, tenant.Value, artifact.Value, generation)
	})
}

// Publish atomically advances the exact artifact generation after completeness
// verification. Exact retries succeed; stale current pointers and changed
// immutable manifests return their ArtifactVault typed errors.
func (r *ArtifactRepository) Publish(ctx context.Context, request contracts.ArtifactPublishRequest) (artifactvault.GenerationRecord, error) {
	if r == nil || r.authority == nil || !validManifest(request.Manifest) || request.ExpectedGeneration == math.MaxUint64 ||
		request.Manifest.Generation != request.ExpectedGeneration+1 {
		return artifactvault.GenerationRecord{}, artifactvault.ErrInvalid
	}
	return writeResult(ctx, r.authority, func(writer localstate.MetadataWriter) (artifactvault.GenerationRecord, error) {
		current, err := currentGeneration(ctx, writer, request.Manifest.Tenant.Value, request.Manifest.Artifact.Value)
		if err != nil {
			return artifactvault.GenerationRecord{}, err
		}
		record, err := loadArtifact(ctx, writer, request.Manifest.Tenant.Value, request.Manifest.Artifact.Value, request.Manifest.Generation)
		if err == nil && record.Status == artifactvault.StatusPublished && current == request.Manifest.Generation {
			if request.ExpectedGeneration == record.Manifest.Generation-1 && sameManifest(record.Manifest, request.Manifest) {
				return record, nil
			}
			return artifactvault.GenerationRecord{}, artifactvault.ErrConflict
		}
		if current != request.ExpectedGeneration {
			return artifactvault.GenerationRecord{}, artifactvault.ErrStaleGeneration
		}
		if err != nil || !validFrames(record.Manifest, record.Frames) {
			return artifactvault.GenerationRecord{}, artifactvault.ErrIncomplete
		}
		if !sameManifest(record.Manifest, request.Manifest) || record.Status != artifactvault.StatusStaged {
			return artifactvault.GenerationRecord{}, artifactvault.ErrConflict
		}
		if _, err := writer.ExecContext(ctx, `UPDATE artifact_manifests SET status='published'
		WHERE tenant_id=? AND artifact_id=? AND generation=? AND status='staged'`,
			record.Manifest.Tenant.Value, record.Manifest.Artifact.Value, record.Manifest.Generation); err != nil {
			return artifactvault.GenerationRecord{}, artifactvault.ErrConflict
		}
		if request.ExpectedGeneration == 0 {
			_, err = writer.ExecContext(ctx, `INSERT INTO artifact_generations
			(tenant_id,artifact_id,current_generation) VALUES (?,?,?)`,
				record.Manifest.Tenant.Value, record.Manifest.Artifact.Value, record.Manifest.Generation)
		} else {
			_, err = writer.ExecContext(ctx, `UPDATE artifact_generations SET current_generation=?
			WHERE tenant_id=? AND artifact_id=? AND current_generation=?`, record.Manifest.Generation,
				record.Manifest.Tenant.Value, record.Manifest.Artifact.Value, request.ExpectedGeneration)
		}
		if err != nil {
			return artifactvault.GenerationRecord{}, artifactvault.ErrConflict
		}
		record.Status = artifactvault.StatusPublished
		return record, nil
	})
}

// Quarantine moves one non-purged generation into fail-closed quarantine.
func (r *ArtifactRepository) Quarantine(ctx context.Context, tenant, artifact contracts.Identifier, generation uint64, reason string) error {
	if r == nil || r.authority == nil || !validID(tenant, "tenant") || !validID(artifact, "artifact") ||
		generation == 0 || reason == "" || len(reason) > 128 {
		return artifactvault.ErrInvalid
	}
	return r.transition(ctx, tenant, artifact, generation,
		[]artifactvault.Status{artifactvault.StatusStaged, artifactvault.StatusPublished, artifactvault.StatusTombstoned, artifactvault.StatusQuarantined},
		artifactvault.StatusQuarantined)
}

// Tombstone immediately denies reads of the exact current published generation.
func (r *ArtifactRepository) Tombstone(ctx context.Context, request contracts.TombstoneRequest) (artifactvault.GenerationRecord, error) {
	if r == nil || r.authority == nil || !validID(request.Tenant, "tenant") || !validID(request.Artifact, "artifact") ||
		request.ExpectedGeneration == 0 || request.ReasonCode == "" || len(request.ReasonCode) > 128 {
		return artifactvault.GenerationRecord{}, artifactvault.ErrInvalid
	}
	return writeResult(ctx, r.authority, func(writer localstate.MetadataWriter) (artifactvault.GenerationRecord, error) {
		current, err := currentGeneration(ctx, writer, request.Tenant.Value, request.Artifact.Value)
		if err != nil {
			return artifactvault.GenerationRecord{}, err
		}
		if current != request.ExpectedGeneration {
			return artifactvault.GenerationRecord{}, artifactvault.ErrStaleGeneration
		}
		record, err := loadArtifact(ctx, writer, request.Tenant.Value, request.Artifact.Value, request.ExpectedGeneration)
		if err != nil {
			return artifactvault.GenerationRecord{}, err
		}
		receipt := canonicalTombstoneReceipt(record)
		if record.Status == artifactvault.StatusTombstoned {
			persistedReason, persistedReceipt, loadErr := loadTombstone(ctx, writer, request.Tenant.Value, request.Artifact.Value, request.ExpectedGeneration)
			if loadErr != nil || persistedReason != request.ReasonCode || persistedReceipt != receipt {
				return artifactvault.GenerationRecord{}, artifactvault.ErrConflict
			}
			return record, nil
		}
		if record.Status != artifactvault.StatusPublished {
			return artifactvault.GenerationRecord{}, artifactvault.ErrConflict
		}
		if _, err := writer.ExecContext(ctx, `UPDATE artifact_manifests SET status='tombstoned'
		WHERE tenant_id=? AND artifact_id=? AND generation=? AND status='published'`,
			request.Tenant.Value, request.Artifact.Value, request.ExpectedGeneration); err != nil {
			return artifactvault.GenerationRecord{}, artifactvault.ErrConflict
		}
		if _, err := writer.ExecContext(ctx, `INSERT INTO artifact_storage_tombstones
			(tenant_id,artifact_id,generation,request_reason_code,receipt_operation_namespace,
			receipt_operation_value,receipt_status,receipt_reason_code,receipt_watermark)
			VALUES (?,?,?,?,?,?,?,?,?)`, request.Tenant.Value, request.Artifact.Value,
			request.ExpectedGeneration, request.ReasonCode, receipt.OperationID.Namespace,
			receipt.OperationID.Value, receipt.Status, receipt.ReasonCode, receipt.Watermark); err != nil {
			return artifactvault.GenerationRecord{}, artifactvault.ErrConflict
		}
		record.Status = artifactvault.StatusTombstoned
		return record, nil
	})
}

// PreparePurge returns the exact tombstoned or already purged generation.
func (r *ArtifactRepository) PreparePurge(ctx context.Context, request contracts.PurgeRequest, generation uint64) (artifactvault.GenerationRecord, error) {
	if r == nil || r.authority == nil || !validID(request.Tenant, "tenant") || !validID(request.Artifact, "artifact") ||
		generation == 0 || request.KeyEpoch == 0 {
		return artifactvault.GenerationRecord{}, artifactvault.ErrInvalid
	}
	return readResult(ctx, r.authority, func(reader queryer) (artifactvault.GenerationRecord, error) {
		record, err := loadArtifact(ctx, reader, request.Tenant.Value, request.Artifact.Value, generation)
		if err != nil || record.Manifest.KeyEpoch != request.KeyEpoch ||
			(record.Status != artifactvault.StatusTombstoned && record.Status != artifactvault.StatusPurged) {
			return artifactvault.GenerationRecord{}, artifactvault.ErrTombstoned
		}
		_, receipt, err := loadTombstone(ctx, reader, request.Tenant.Value, request.Artifact.Value, generation)
		if err != nil || receipt != request.TombstoneReceipt || receipt != canonicalTombstoneReceipt(record) {
			return artifactvault.GenerationRecord{}, artifactvault.ErrTombstoned
		}
		return record, nil
	})
}

// CompletePurge marks one exact fenced tombstoned generation as purged. It is an
// idempotent metadata acknowledgement after the caller deletes every object.
func (r *ArtifactRepository) CompletePurge(ctx context.Context, record artifactvault.GenerationRecord) error {
	if r == nil || r.authority == nil || !validManifest(record.Manifest) || record.Fence == 0 {
		return artifactvault.ErrInvalid
	}
	return writeOnly(ctx, r.authority, func(writer localstate.MetadataWriter) error {
		existing, err := loadArtifact(ctx, writer, record.Manifest.Tenant.Value, record.Manifest.Artifact.Value, record.Manifest.Generation)
		if err != nil || existing.Fence != record.Fence {
			return artifactvault.ErrConflict
		}
		if existing.Status == artifactvault.StatusPurged {
			return nil
		}
		if existing.Status != artifactvault.StatusTombstoned {
			return artifactvault.ErrConflict
		}
		if _, err := writer.ExecContext(ctx, `UPDATE artifact_manifests SET status='purged'
		WHERE tenant_id=? AND artifact_id=? AND generation=? AND status='tombstoned'`,
			record.Manifest.Tenant.Value, record.Manifest.Artifact.Value, record.Manifest.Generation); err != nil {
			return artifactvault.ErrConflict
		}
		return nil
	})
}

func (r *ArtifactRepository) transition(ctx context.Context, tenant, artifact contracts.Identifier, generation uint64, allowed []artifactvault.Status, target artifactvault.Status) error {
	return writeOnly(ctx, r.authority, func(writer localstate.MetadataWriter) error {
		record, err := loadArtifact(ctx, writer, tenant.Value, artifact.Value, generation)
		if err != nil {
			return err
		}
		if !containsStatus(allowed, record.Status) {
			return artifactvault.ErrConflict
		}
		if record.Status == target {
			return nil
		}
		if _, err := writer.ExecContext(ctx, `UPDATE artifact_manifests SET status=?
		WHERE tenant_id=? AND artifact_id=? AND generation=?`, target, tenant.Value, artifact.Value, generation); err != nil {
			return artifactvault.ErrConflict
		}
		return nil
	})
}

func canonicalTombstoneReceipt(record artifactvault.GenerationRecord) contracts.Receipt {
	return contracts.Receipt{
		OperationID: contracts.Identifier{Namespace: "artifact-operation", Value: encodeComposite(record.Manifest.Artifact.Value, strconv.FormatUint(record.Manifest.Generation, 10))},
		Status:      "tombstoned", ReasonCode: "OURO-ARTIFACT-TOMBSTONED", Watermark: record.Manifest.Generation,
	}
}

func loadTombstone(ctx context.Context, query queryer, tenant, artifact string, generation uint64) (string, contracts.Receipt, error) {
	var reason string
	var receipt contracts.Receipt
	err := query.QueryRowContext(ctx, `SELECT request_reason_code,receipt_operation_namespace,
		receipt_operation_value,receipt_status,receipt_reason_code,receipt_watermark
		FROM artifact_storage_tombstones WHERE tenant_id=? AND artifact_id=? AND generation=?`,
		tenant, artifact, generation).Scan(&reason, &receipt.OperationID.Namespace, &receipt.OperationID.Value,
		&receipt.Status, &receipt.ReasonCode, &receipt.Watermark)
	if errors.Is(err, sql.ErrNoRows) {
		return "", contracts.Receipt{}, artifactvault.ErrNotFound
	}
	if err != nil {
		return "", contracts.Receipt{}, operationError(ctx, "read artifact tombstone")
	}
	return reason, receipt, nil
}

func encodeComposite(parts ...string) string {
	var encoded strings.Builder
	for _, part := range parts {
		encoded.WriteString(strconv.Itoa(len(part)))
		encoded.WriteByte(':')
		encoded.WriteString(part)
	}
	return encoded.String()
}

func sameFrames(left, right []artifactvault.FrameRecord) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func containsStatus(statuses []artifactvault.Status, status artifactvault.Status) bool {
	for _, candidate := range statuses {
		if candidate == status {
			return true
		}
	}
	return false
}
