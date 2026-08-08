package localstorage

import (
	"context"
	"database/sql"
	"errors"
	"math"

	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/artifactvault"
	"github.com/sltbrta/sentra-code-memory-v2/services/internal/contracts"
)

type queryer interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func loadArtifact(ctx context.Context, query queryer, tenant, artifact string, generation uint64) (artifactvault.GenerationRecord, error) {
	var record artifactvault.GenerationRecord
	var digestAlgorithm, digestHex, status string
	var length, frameCount, keyEpoch, fence int64
	digestAlgorithm = "sha256"
	err := query.QueryRowContext(ctx, `SELECT manifest.content_digest,manifest.byte_length,
		manifest.frame_count,manifest.key_epoch,reservation.locator,reservation.reservation_fence,manifest.status
		FROM artifact_manifests AS manifest JOIN artifact_reservations AS reservation
		ON reservation.tenant_id=manifest.tenant_id AND reservation.artifact_id=manifest.artifact_id
		AND reservation.generation=manifest.generation
		WHERE manifest.tenant_id=? AND manifest.artifact_id=? AND manifest.generation=?`, tenant, artifact, generation).
		Scan(&digestHex, &length, &frameCount, &keyEpoch, &record.Locator, &fence, &status)
	if errors.Is(err, sql.ErrNoRows) {
		return artifactvault.GenerationRecord{}, artifactvault.ErrNotFound
	}
	if err != nil || length <= 0 || frameCount <= 0 || frameCount > maxFrameCount || keyEpoch <= 0 ||
		fence <= 0 || !validLocator(record.Locator) {
		return artifactvault.GenerationRecord{}, operationError(ctx, "read artifact manifest")
	}
	record.Manifest = contracts.ArtifactManifest{
		Tenant:     contracts.Identifier{Namespace: "tenant", Value: tenant},
		Artifact:   contracts.Identifier{Namespace: "artifact", Value: artifact},
		Digest:     contracts.Digest{Algorithm: digestAlgorithm, Hex: digestHex},
		Generation: generation, KeyEpoch: uint64(keyEpoch), Length: uint64(length), FrameCount: uint32(frameCount),
	}
	record.Status = artifactvault.Status(status)
	record.Fence = uint64(fence)
	if !validManifest(record.Manifest) || !containsStatus([]artifactvault.Status{
		artifactvault.StatusStaged, artifactvault.StatusPublished, artifactvault.StatusTombstoned,
		artifactvault.StatusPurged, artifactvault.StatusQuarantined,
	}, record.Status) {
		return artifactvault.GenerationRecord{}, artifactvault.ErrCorrupt
	}
	rows, err := query.QueryContext(ctx, `SELECT frame_index,offset_bytes,length_bytes,frame_digest FROM artifact_frames
		WHERE tenant_id=? AND artifact_id=? AND generation=? ORDER BY frame_index LIMIT ?`,
		tenant, artifact, generation, maxFrameCount+1)
	if err != nil {
		return artifactvault.GenerationRecord{}, operationError(ctx, "read artifact frames")
	}
	defer rows.Close()
	for rows.Next() {
		var frame artifactvault.FrameRecord
		var index, offset, frameLength int64
		frame.ObjectDigest.Algorithm = "sha256"
		if err := rows.Scan(&index, &offset, &frameLength, &frame.ObjectDigest.Hex); err != nil ||
			index < 0 || offset < 0 || frameLength <= 0 || index > math.MaxUint32 || frameLength > math.MaxUint32 {
			return artifactvault.GenerationRecord{}, operationError(ctx, "scan artifact frame")
		}
		frame.Index, frame.Offset, frame.Length = uint32(index), uint64(offset), uint32(frameLength)
		record.Frames = append(record.Frames, frame)
		if len(record.Frames) > int(record.Manifest.FrameCount) {
			return artifactvault.GenerationRecord{}, artifactvault.ErrCorrupt
		}
	}
	if err := rows.Err(); err != nil {
		return artifactvault.GenerationRecord{}, operationError(ctx, "iterate artifact frames")
	}
	if len(record.Frames) != 0 && !validFrames(record.Manifest, record.Frames) {
		return artifactvault.GenerationRecord{}, artifactvault.ErrCorrupt
	}
	return record, nil
}

func currentGeneration(ctx context.Context, query queryer, tenant, artifact string) (uint64, error) {
	var generation int64
	err := query.QueryRowContext(ctx, `SELECT current_generation FROM artifact_generations
		WHERE tenant_id=? AND artifact_id=?`, tenant, artifact).Scan(&generation)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil || generation <= 0 {
		return 0, operationError(ctx, "read current generation")
	}
	return uint64(generation), nil
}
