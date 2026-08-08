package localauthority

import (
	"time"

	shared "github.com/sltbrta/sentra-code-memory-v2/services/internal/contracts"
)

// IngestionConfig enables one bounded committed-Git source. The root and Git
// executable must be absolute; all limits and CommandTimeout must be positive.
// The policy is deliberately fixed to both root ignore files and no symlink
// following so callers cannot silently weaken source admission.
type IngestionConfig struct {
	ApprovedRoot          string
	GitExecutable         string
	RepositoryID          string
	CommandTimeout        time.Duration
	MaxFiles              int
	MaxPathBytes          int
	MaxFileBytes          int64
	MaxTotalBytes         int64
	MaxIdempotencyRecords int
}

// IngestionPolicyBothIgnoreNoFollow is the sole Stage 3 source policy.
const IngestionPolicyBothIgnoreNoFollow = "both_ignore_files_no_follow"

// IngestionContext pins the authenticated actor, runtime configuration,
// source policy, command fence, and current authorization check.
type IngestionContext struct {
	Identity            Identity
	ConfigurationDigest Digest
	Policy              string
	Fence               uint64
	Authorize           AuthorizeFunc
}

// AddSourceRequest admits the exact committed tree as generation one.
type AddSourceRequest struct {
	IngestionContext
	ExpectedCommitOID string
	IdempotencyKey    string
}

// SourceStatusRequest asks for the configured source's path-free lifecycle.
type SourceStatusRequest struct{ IngestionContext }

// ReconcileSourceRequest publishes one exact commit transition.
type ReconcileSourceRequest struct {
	IngestionContext
	ExpectedGenerationID string
	ExpectedCommitOID    string
	TargetCommitOID      string
	IdempotencyKey       string
}

// RevokeSourceRequest immediately denies and tombstones a current generation.
type RevokeSourceRequest struct {
	IngestionContext
	ExpectedGenerationID string
	RevocationEpoch      uint64
	IdempotencyKey       string
}

// SearchKind selects one deterministic bounded code-index query.
type SearchKind string

const (
	// SearchExact returns every occurrence with the exact spelling.
	SearchExact SearchKind = "exact"
	// SearchSymbol returns exact definitions only.
	SearchSymbol SearchKind = "symbol"
	// SearchReference returns exact references only.
	SearchReference SearchKind = "reference"
)

// SearchCodeRequest queries only an exact current complete generation.
type SearchCodeRequest struct {
	IngestionContext
	GenerationID string
	Query        string
	Kind         SearchKind
	Limit        uint32
	Cursor       string
}

// SourceStatus is the bounded, path-free current lifecycle and readiness view.
type SourceStatus struct {
	SourceID            string
	GenerationID        string
	SnapshotID          string
	CommitOID           string
	TreeOID             string
	PolicyDigest        Digest
	Sequence            uint64
	State               string
	Readiness           []LanguageReadiness
	Revoked             bool
	ConfigurationDigest Digest
}

// LanguageReadiness reports one complete P5 syntax or lexical lane.
type LanguageReadiness struct {
	Language   string
	Coverage   string
	ReasonCode string
}

// IngestionResult is the durable publication disposition and current status.
type IngestionResult struct {
	Receipt  shared.Receipt
	Status   SourceStatus
	Replayed bool
}

// CodeMatch is one immutable exact committed-source fact. Content is only the
// matched source spelling, never an unrestricted file body.
type CodeMatch struct {
	Path           string
	BlobOID        string
	ContentDigest  string
	RevisionID     string
	SourceObjectID string
	ByteLength     uint64
	MediaType      string
	Language       string
	Coverage       string
	Kind           SearchKind
	StartLine      uint32
	StartColumn    uint32
	EndLine        uint32
	EndColumn      uint32
	Content        string
}

// SearchCodeResult is one stable page pinned to GenerationID.
type SearchCodeResult struct {
	GenerationID string
	Matches      []CodeMatch
	NextCursor   string
}
