// Package artifactvault owns immutable encrypted artifact bytes and their publication lifecycle.
// It exposes narrow staging, range hydration, reconciliation, tombstone, and purge operations;
// callers never receive filesystem paths or a generic object-store capability.
package artifactvault

import (
	"context"
	"errors"
	"io"

	"github.com/sltbrta/sentra-code-memory-v2/services/internal/contracts"
)

const (
	defaultFrameBytes = 64 * 1024
	defaultMaxRead    = 16 * 1024 * 1024
	maxArtifactBytes  = 1 << 40
	maxFrameBytes     = 16 * 1024 * 1024
	maxReadBytes      = 64 * 1024 * 1024
	maxFrameCount     = 1_048_576
	maxObjectBytes    = maxFrameBytes + frameHeaderBytes + 16
)

var (
	// ErrInvalid reports a malformed, empty, oversized, or out-of-bounds request.
	ErrInvalid = errors.New("artifactvault: invalid request")
	// ErrNotFound reports an absent generation without disclosing another tenant's state.
	ErrNotFound = errors.New("artifactvault: generation not found")
	// ErrConflict reports a duplicate with different content or an active concurrent stage.
	ErrConflict = errors.New("artifactvault: immutable conflict")
	// ErrStaleGeneration reports a failed current-generation compare-and-swap.
	ErrStaleGeneration = errors.New("artifactvault: stale generation")
	// ErrIncomplete reports a generation that cannot be published or hydrated completely.
	ErrIncomplete = errors.New("artifactvault: generation incomplete")
	// ErrCorrupt reports failed digest, framing, or authenticated-decryption verification.
	ErrCorrupt = errors.New("artifactvault: encrypted material corrupt")
	// ErrQuarantined reports material denied after an unreadable key or integrity anomaly.
	ErrQuarantined = errors.New("artifactvault: generation quarantined")
	// ErrTombstoned reports a generation denied before physical purge.
	ErrTombstoned = errors.New("artifactvault: generation tombstoned")
)

// Status is the forward-only local artifact lifecycle.
type Status string

const (
	StatusStaged      Status = "staged"
	StatusPublished   Status = "published"
	StatusTombstoned  Status = "tombstoned"
	StatusPurged      Status = "purged"
	StatusQuarantined Status = "quarantined"
)

// FrameRecord binds one encrypted frame object to its plaintext range and object digest.
type FrameRecord struct {
	Index        uint32
	Offset       uint64
	Length       uint32
	ObjectDigest contracts.Digest
}

// GenerationRecord is metadata only. Locator is opaque and must never be logged or exposed.
type GenerationRecord struct {
	Manifest contracts.ArtifactManifest
	Locator  string
	Frames   []FrameRecord
	Status   Status
	Fence    uint64
}

// Repository is the SQLite-shaped metadata boundary. Implementations must serialize
// generation CAS and never persist payload, ciphertext, DEK, or root-key bytes.
type Repository interface {
	BeginStage(context.Context, contracts.ArtifactStageRequest, string) (GenerationRecord, bool, error)
	CompleteStage(context.Context, GenerationRecord) error
	AbortStage(context.Context, GenerationRecord) error
	Get(context.Context, contracts.Identifier, contracts.Identifier, uint64) (GenerationRecord, error)
	Publish(context.Context, contracts.ArtifactPublishRequest) (GenerationRecord, error)
	Quarantine(context.Context, contracts.Identifier, contracts.Identifier, uint64, string) error
	Tombstone(context.Context, contracts.TombstoneRequest) (GenerationRecord, error)
	PreparePurge(context.Context, contracts.PurgeRequest, uint64) (GenerationRecord, error)
	CompletePurge(context.Context, GenerationRecord) error
}

// Options bounds allocation and permits deterministic cryptographic randomness in tests.
type Options struct {
	FrameBytes   uint32
	MaxReadBytes uint64
	Random       io.Reader
}

// HydratedRange combines frozen-port metadata with authenticated plaintext bytes.
// Bytes is freshly allocated and bounded by Options.MaxReadBytes.
type HydratedRange struct {
	Metadata contracts.ArtifactReadResult
	Bytes    []byte
}

// MigrationRequest explicitly imports authorized legacy plaintext into a new encrypted generation.
// It does not delete or claim erasure of the legacy source; deletion remains an upstream obligation.
type MigrationRequest struct {
	Stage contracts.ArtifactStageRequest
}
