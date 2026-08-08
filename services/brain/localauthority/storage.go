package localauthority

import (
	"context"
	"errors"
	"io"

	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/artifactvault"
	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/evidenceledger"
	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/keyring"
	shared "github.com/sltbrta/sentra-code-memory-v2/services/internal/contracts"
)

// ArtifactRepository persists encrypted artifact generation metadata. Runtime
// composition must provide a durable implementation; memory implementations are
// suitable only for tests.
type ArtifactRepository = artifactvault.Repository

// EvidenceRepository persists tenant-and-brain-scoped evidence metadata.
type EvidenceRepository = evidenceledger.Repository

const (
	// RootKeyBytes is the required transient AES-256 root-key size.
	RootKeyBytes = keyring.RootKeyBytes
)

// KeyMaterial combines an opaque reference with transient root bytes. It is a
// public alias so outer composition packages can implement KeyResolver without
// importing a brain-internal package. Callers must clear RootKey promptly.
type KeyMaterial = keyring.Material

// KeyResolver returns transient tenant-scoped key material. Production must use
// a secret-backed resolver; there is deliberately no file-key fallback. The
// interface exposes resolution only, never secret creation or storage effects.
type KeyResolver interface {
	Current(context.Context, Identifier) (KeyMaterial, error)
	Resolve(context.Context, Identifier, uint64) (KeyMaterial, error)
}

// StorageOptions bounds independently authenticated frames and hydrated reads.
type StorageOptions struct {
	FrameBytes   uint32
	MaxReadBytes uint64
	Random       io.Reader
}

// Storage owns the encrypted ArtifactVault object descriptor and evidence
// ledger. Close releases the retained object-root descriptor.
type Storage struct {
	vault     *artifactvault.Vault
	artifacts ArtifactRepository
	evidence  *evidenceledger.Ledger
	objects   *artifactvault.LocalStore
}

// NewStorage composes the real encrypted ArtifactVault with injected metadata,
// key, and evidence ports. It never creates key material or substitutes a
// memory repository. Callers own durable adapter selection.
func NewStorage(
	objectRoot string,
	artifacts ArtifactRepository,
	keys KeyResolver,
	evidence EvidenceRepository,
	options StorageOptions,
) (*Storage, error) {
	if artifacts == nil || keys == nil || evidence == nil {
		return nil, ErrInvalid
	}
	objects, err := artifactvault.NewLocalStore(objectRoot)
	if err != nil {
		return nil, ErrInvalid
	}
	vault, err := artifactvault.New(objects, artifacts, keys, artifactvault.Options{
		FrameBytes: options.FrameBytes, MaxReadBytes: options.MaxReadBytes, Random: options.Random,
	})
	if err != nil {
		_ = objects.Close()
		return nil, ErrInvalid
	}
	ledger, err := evidenceledger.New(evidence)
	if err != nil {
		_ = objects.Close()
		return nil, ErrInvalid
	}
	return &Storage{vault: vault, artifacts: artifacts, evidence: ledger, objects: objects}, nil
}

// Close releases the encrypted object-root descriptor after request handling stops.
func (s *Storage) Close() error {
	if s == nil || s.objects == nil {
		return nil
	}
	return s.objects.Close()
}

func (s *Storage) stage(ctx context.Context, artifact Artifact, content io.Reader) error {
	_, err := s.vault.StageContent(ctx, stageRequest(artifact), content)
	return err
}

func (s *Storage) publish(ctx context.Context, artifact Artifact, brain Identifier) error {
	_, err := s.vault.Publish(ctx, shared.ArtifactPublishRequest{
		Manifest: manifest(artifact), ExpectedGeneration: artifact.ExpectedGeneration,
	})
	if err != nil {
		return err
	}
	_, err = s.evidence.Admit(ctx, evidenceledger.Record{
		Tenant: artifact.Tenant, Brain: brain,
		Evidence: evidenceID(artifact.ID), Artifact: artifact.ID,
		Generation: artifact.Generation, Anchor: "artifact:full", Digest: artifact.Digest,
	})
	return err
}

func (s *Storage) read(ctx context.Context, artifact Artifact, offset, length uint64) (artifactvault.HydratedRange, error) {
	return s.vault.HydrateRange(ctx, shared.ArtifactReadRequest{
		Artifact: artifact.ID, Tenant: artifact.Tenant, Generation: artifact.Generation,
		Range: shared.ByteRange{Offset: offset, Length: length},
	})
}

// canonicalArtifact resolves immutable generation metadata for every non-admit
// command. Newly accepted operations require published metadata; only completed
// delete replays may resolve tombstoned or purged metadata. It rejects
// request/metadata disagreement and replaces only the key epoch and immutable
// layout with repository-authoritative values. Admission does not call this
// method because it alone uses the configured current epoch.
func (s *Storage) canonicalArtifact(ctx context.Context, artifact Artifact, allowDeletedReplay bool) (Artifact, error) {
	if s == nil || s.artifacts == nil {
		return Artifact{}, ErrUnavailable
	}
	record, err := s.artifacts.Get(ctx, artifact.Tenant, artifact.ID, artifact.Generation)
	if err != nil {
		switch {
		case errors.Is(err, artifactvault.ErrNotFound):
			return Artifact{}, ErrDenied
		case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
			return Artifact{}, err
		default:
			return Artifact{}, ErrUnavailable
		}
	}
	if record.Status != artifactvault.StatusPublished &&
		(!allowDeletedReplay || (record.Status != artifactvault.StatusTombstoned && record.Status != artifactvault.StatusPurged)) {
		return Artifact{}, ErrDenied
	}
	manifest := record.Manifest
	if manifest.Tenant != artifact.Tenant || manifest.Artifact != artifact.ID ||
		manifest.Generation != artifact.Generation || manifest.Digest != artifact.Digest ||
		manifest.KeyEpoch == 0 {
		return Artifact{}, ErrDenied
	}
	artifact.KeyEpoch = manifest.KeyEpoch
	artifact.Length = manifest.Length
	artifact.FrameCount = manifest.FrameCount
	return artifact, nil
}

func (s *Storage) delete(ctx context.Context, artifact Artifact, brain Identifier, purge bool) error {
	tombstone, err := s.vault.Tombstone(ctx, shared.TombstoneRequest{
		Artifact: artifact.ID, Tenant: artifact.Tenant,
		ExpectedGeneration: artifact.ExpectedGeneration, ReasonCode: "user_delete",
	})
	if err != nil {
		return err
	}
	// Vault denial is authoritative first. ErrNotFound from evidence is safe on
	// retry because a prior tombstone has already made the bytes unreadable.
	if err := s.evidence.Tombstone(ctx, artifact.Tenant, brain, evidenceID(artifact.ID)); err != nil &&
		!errors.Is(err, evidenceledger.ErrNotFound) {
		return err
	}
	if !purge {
		return nil
	}
	_, err = s.vault.Purge(ctx, shared.PurgeRequest{
		Artifact: artifact.ID, Tenant: artifact.Tenant,
		TombstoneReceipt: tombstone.Receipt, KeyEpoch: artifact.KeyEpoch,
	})
	return err
}

func stageRequest(artifact Artifact) shared.ArtifactStageRequest {
	return shared.ArtifactStageRequest{Manifest: manifest(artifact), ExpectedGeneration: artifact.ExpectedGeneration}
}

func manifest(artifact Artifact) shared.ArtifactManifest {
	return shared.ArtifactManifest{
		Artifact: artifact.ID, Tenant: artifact.Tenant, Digest: artifact.Digest,
		Generation: artifact.Generation, KeyEpoch: artifact.KeyEpoch,
		Length: artifact.Length, FrameCount: artifact.FrameCount,
	}
}

func evidenceID(artifact Identifier) Identifier {
	return Identifier{Namespace: "evidence", Value: artifact.Value}
}
