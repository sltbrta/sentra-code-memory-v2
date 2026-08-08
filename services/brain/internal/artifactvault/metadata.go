package artifactvault

import (
	"context"
	"strconv"
	"sync"

	"github.com/sltbrta/sentra-code-memory-v2/services/internal/contracts"
)

// MemoryRepository is a deterministic-shape, concurrency-safe metadata adapter for tests.
// Production composition supplies the Stage 2 SQLite implementation through Repository.
type MemoryRepository struct {
	mu      sync.Mutex
	records map[string]GenerationRecord
	current map[string]uint64
	next    uint64
}

// NewMemoryRepository returns an empty isolated metadata repository.
func NewMemoryRepository() *MemoryRepository {
	return &MemoryRepository{
		records: make(map[string]GenerationRecord),
		current: make(map[string]uint64),
	}
}

// BeginStage reserves an exact next generation and returns an identical completed
// stage as an idempotent duplicate. A concurrent in-progress stage conflicts.
func (r *MemoryRepository) BeginStage(_ context.Context, request contracts.ArtifactStageRequest, locator string) (GenerationRecord, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	current := r.current[artifactKey(request.Manifest.Tenant, request.Manifest.Artifact)]
	if current != request.ExpectedGeneration || request.Manifest.Generation != current+1 {
		return GenerationRecord{}, false, ErrStaleGeneration
	}
	key := generationKey(request.Manifest.Tenant, request.Manifest.Artifact, request.Manifest.Generation)
	if existing, ok := r.records[key]; ok {
		if !sameManifest(existing.Manifest, request.Manifest) {
			return GenerationRecord{}, false, ErrConflict
		}
		if len(existing.Frames) == int(existing.Manifest.FrameCount) {
			return cloneRecord(existing), true, nil
		}
		return GenerationRecord{}, false, ErrConflict
	}
	r.next++
	record := GenerationRecord{Manifest: request.Manifest, Locator: locator, Status: StatusStaged, Fence: r.next}
	r.records[key] = record
	return cloneRecord(record), false, nil
}

// CompleteStage atomically records the complete immutable frame manifest for its reservation fence.
func (r *MemoryRepository) CompleteStage(_ context.Context, record GenerationRecord) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	key := generationKey(record.Manifest.Tenant, record.Manifest.Artifact, record.Manifest.Generation)
	existing, ok := r.records[key]
	if !ok || existing.Fence != record.Fence || existing.Status != StatusStaged {
		return ErrConflict
	}
	if len(record.Frames) != int(record.Manifest.FrameCount) {
		return ErrIncomplete
	}
	existing.Frames = append([]FrameRecord(nil), record.Frames...)
	r.records[key] = existing
	return nil
}

// AbortStage removes only the caller's still-incomplete fenced reservation.
func (r *MemoryRepository) AbortStage(_ context.Context, record GenerationRecord) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	key := generationKey(record.Manifest.Tenant, record.Manifest.Artifact, record.Manifest.Generation)
	existing, ok := r.records[key]
	if !ok {
		return nil
	}
	if existing.Fence != record.Fence || len(existing.Frames) != 0 || existing.Status != StatusStaged {
		return ErrConflict
	}
	delete(r.records, key)
	return nil
}

// Get returns copied metadata for one exact tenant/artifact/generation tuple.
func (r *MemoryRepository) Get(_ context.Context, tenant, artifact contracts.Identifier, generation uint64) (GenerationRecord, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	record, ok := r.records[generationKey(tenant, artifact, generation)]
	if !ok {
		return GenerationRecord{}, ErrNotFound
	}
	return cloneRecord(record), nil
}

// Publish conditionally advances the current pointer after completeness checks.
func (r *MemoryRepository) Publish(_ context.Context, request contracts.ArtifactPublishRequest) (GenerationRecord, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	artifact := artifactKey(request.Manifest.Tenant, request.Manifest.Artifact)
	key := generationKey(request.Manifest.Tenant, request.Manifest.Artifact, request.Manifest.Generation)
	if record, ok := r.records[key]; ok && record.Status == StatusPublished && r.current[artifact] == record.Manifest.Generation {
		if request.ExpectedGeneration != record.Manifest.Generation-1 || !sameManifest(record.Manifest, request.Manifest) {
			return GenerationRecord{}, ErrConflict
		}
		return cloneRecord(record), nil
	}
	if r.current[artifact] != request.ExpectedGeneration || request.Manifest.Generation != request.ExpectedGeneration+1 {
		return GenerationRecord{}, ErrStaleGeneration
	}
	record, ok := r.records[key]
	if !ok || len(record.Frames) != int(record.Manifest.FrameCount) {
		return GenerationRecord{}, ErrIncomplete
	}
	if !sameManifest(record.Manifest, request.Manifest) {
		return GenerationRecord{}, ErrConflict
	}
	if record.Status != StatusStaged {
		return GenerationRecord{}, ErrConflict
	}
	record.Status = StatusPublished
	r.records[key] = record
	r.current[artifact] = record.Manifest.Generation
	return cloneRecord(record), nil
}

// Quarantine applies a fail-closed terminal read state to staged or published material.
func (r *MemoryRepository) Quarantine(_ context.Context, tenant, artifact contracts.Identifier, generation uint64, _ string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	key := generationKey(tenant, artifact, generation)
	record, ok := r.records[key]
	if !ok {
		return ErrNotFound
	}
	if record.Status == StatusPurged {
		return ErrConflict
	}
	record.Status = StatusQuarantined
	r.records[key] = record
	return nil
}

// Tombstone immediately denies reads of the exact current generation.
func (r *MemoryRepository) Tombstone(_ context.Context, request contracts.TombstoneRequest) (GenerationRecord, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	artifact := artifactKey(request.Tenant, request.Artifact)
	if request.ExpectedGeneration == 0 || r.current[artifact] != request.ExpectedGeneration {
		return GenerationRecord{}, ErrStaleGeneration
	}
	key := generationKey(request.Tenant, request.Artifact, request.ExpectedGeneration)
	record, ok := r.records[key]
	if !ok {
		return GenerationRecord{}, ErrNotFound
	}
	if record.Status == StatusTombstoned {
		return cloneRecord(record), nil
	}
	if record.Status != StatusPublished {
		return GenerationRecord{}, ErrConflict
	}
	record.Status = StatusTombstoned
	r.records[key] = record
	return cloneRecord(record), nil
}

// PreparePurge returns the unique exact tombstoned generation without changing state.
func (r *MemoryRepository) PreparePurge(_ context.Context, request contracts.PurgeRequest, generation uint64) (GenerationRecord, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	found, ok := r.records[generationKey(request.Tenant, request.Artifact, generation)]
	if !ok || found.Manifest.KeyEpoch != request.KeyEpoch || (found.Status != StatusTombstoned && found.Status != StatusPurged) {
		return GenerationRecord{}, ErrTombstoned
	}
	return cloneRecord(found), nil
}

// CompletePurge records purged only after every primary object deletion succeeds.
func (r *MemoryRepository) CompletePurge(_ context.Context, record GenerationRecord) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	key := generationKey(record.Manifest.Tenant, record.Manifest.Artifact, record.Manifest.Generation)
	existing, ok := r.records[key]
	if !ok || existing.Fence != record.Fence {
		return ErrConflict
	}
	if existing.Status == StatusPurged {
		return nil
	}
	if existing.Status != StatusTombstoned {
		return ErrConflict
	}
	existing.Status = StatusPurged
	r.records[key] = existing
	return nil
}

func artifactKey(tenant, artifact contracts.Identifier) string {
	return encodeComposite(tenant.Value, artifact.Value)
}

func generationKey(tenant, artifact contracts.Identifier, generation uint64) string {
	return encodeComposite(tenant.Value, artifact.Value, uintString(generation))
}

func sameManifest(left, right contracts.ArtifactManifest) bool {
	return left == right
}

func cloneRecord(record GenerationRecord) GenerationRecord {
	record.Frames = append([]FrameRecord(nil), record.Frames...)
	return record
}

func uintString(value uint64) string {
	return strconv.FormatUint(value, 10)
}
