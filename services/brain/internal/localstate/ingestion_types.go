// This file defines the narrow Stage 03 ingestion persistence contract.
// Types contain only canonical, path-free metadata; Git hydration and path
// reconstruction remain responsibilities of the composing brain runtime.
package localstate

import (
	"errors"

	"github.com/sltbrta/sentra-code-memory-v2/services/internal/contracts"
)

const (
	// IngestionPublishCommand is the only command type accepted by PublishGeneration.
	IngestionPublishCommand = "ingestion.publish_generation"
	// IngestionRevokeCommand is the only command type accepted by RevokeIngestionSource.
	IngestionRevokeCommand = "ingestion.revoke_source"
)

var (
	// ErrIngestionConflict reports immutable metadata or idempotency disagreement.
	ErrIngestionConflict = errors.New("localstate: ingestion conflict")
	// ErrIngestionStale reports a failed current-generation compare-and-swap.
	ErrIngestionStale = errors.New("localstate: stale ingestion generation")
	// ErrIngestionRevoked reports an operation against a revoked source.
	ErrIngestionRevoked = errors.New("localstate: ingestion source revoked")
)

// IngestionScope identifies one tenant/brain/source domain. Tenant and Brain
// namespaces must be `tenant` and `brain`; SourceID is opaque and path-free.
type IngestionScope struct {
	Tenant   contracts.Identifier
	Brain    contracts.Identifier
	SourceID string
}

// IngestionSourceMetadata freezes the immutable source and approved-root facts
// plus the ACL epoch that applies to all revisions in a publication.
type IngestionSourceMetadata struct {
	RepositoryID        string
	ConfigurationDigest string
	IgnorePolicyDigest  string
	ApprovedRootID      string
	ACLEpoch            uint64
}

// IngestionSnapshotMetadata identifies one complete committed Git snapshot.
// It deliberately excludes absolute and repository-relative paths.
type IngestionSnapshotMetadata struct {
	SnapshotID     string
	CommitOID      string
	TreeOID        string
	PolicyDigest   string
	SnapshotDigest string
}

// IngestionRevisionMetadata is one immutable path-free source revision and its
// snapshot membership. PredecessorRevisionID may name an earlier active row.
type IngestionRevisionMetadata struct {
	RevisionID            string
	SourceObjectID        string
	PathDigest            string
	GitBlobOID            string
	ContentDigest         string
	ByteLength            int64
	EntryKind             string
	MediaType             string
	Language              string
	PredecessorRevisionID string
}

// IngestionReadiness records one P5 lane's complete publication disposition.
// Coverage is `syntax_aware` or `lexical_degraded`; pending lanes cannot publish.
type IngestionReadiness struct {
	Language   string
	Coverage   string
	ReasonCode string
}

// GenerationPublication is one atomic complete-generation write. Sequence one
// requires an empty ExpectedCurrentGenerationID; later sequences require the
// exact currently published generation. State is `ready` or `degraded`.
type GenerationPublication struct {
	Command                     contracts.CommandRecord
	Scope                       IngestionScope
	Source                      IngestionSourceMetadata
	Snapshot                    IngestionSnapshotMetadata
	GenerationID                string
	Sequence                    uint64
	ExpectedCurrentGenerationID string
	State                       string
	SourceWatermark             uint64
	Revisions                   []IngestionRevisionMetadata
	Readiness                   []IngestionReadiness
}

// IngestionCheckpoint is the path-free restart view for one source. A revoked
// checkpoint intentionally retains immutable Git OIDs and generation identity
// so the runtime can report lifecycle state without hydrating source bytes.
type IngestionCheckpoint struct {
	Scope               IngestionScope
	RepositoryID        string
	ConfigurationDigest string
	IgnorePolicyDigest  string
	ApprovedRootID      string
	ACLEpoch            uint64
	RevocationEpoch     uint64
	Revoked             bool
	Tombstoned          bool
	GenerationID        string
	GenerationSequence  uint64
	SnapshotID          string
	CommitOID           string
	TreeOID             string
	PolicyDigest        string
	SnapshotDigest      string
	SourceWatermark     uint64
	GenerationState     string
	// PreviousGenerationID and PreviousCommitOID are populated only for the
	// bounded second generation, allowing deterministic restart reconciliation.
	PreviousGenerationID string
	PreviousCommitOID    string
}

// IngestionExecution is the canonical publication or revocation disposition.
// Exact retries return the original receipt with Replayed set.
type IngestionExecution struct {
	Receipt    contracts.Receipt
	Checkpoint IngestionCheckpoint
	Replayed   bool
}

// IngestionCheckpointQuery authenticates a path-free restart lookup. Identity
// must match an open persisted session and the scope tenant exactly.
type IngestionCheckpointQuery struct {
	Identity contracts.MappedIdentityFact
	Scope    IngestionScope
}

// IngestionRevocation atomically denies a source, removes its current pointer,
// tombstones active revisions, and records source/revision tombstones.
type IngestionRevocation struct {
	Command                     contracts.CommandRecord
	Scope                       IngestionScope
	ExpectedCurrentGenerationID string
	RevocationEpoch             uint64
	ReasonCode                  string
}
