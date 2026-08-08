package ingestion

import (
	"errors"
	"time"
)

var (
	// ErrInvalidInput identifies a missing or malformed caller-controlled value.
	ErrInvalidInput = errors.New("ingestion: invalid input")
	// ErrUnsupportedPolicy identifies an ignore or symlink rule outside bounded v1.
	ErrUnsupportedPolicy = errors.New("ingestion: unsupported policy")
	// ErrOutOfRoot identifies an absolute, escaping, or otherwise unsafe path.
	ErrOutOfRoot = errors.New("ingestion: path outside approved root")
	// ErrStaleGeneration identifies a compare-and-swap against an old generation.
	ErrStaleGeneration = errors.New("ingestion: stale generation")
	// ErrRevoked identifies an operation denied by the immediate revoke overlay.
	ErrRevoked = errors.New("ingestion: source revoked")
	// ErrTombstoned identifies an operation against deleted source metadata.
	ErrTombstoned = errors.New("ingestion: source tombstoned")
	// ErrGit identifies a static Git plumbing failure without exposing stderr.
	ErrGit = errors.New("ingestion: git operation failed")
	// ErrLimit identifies a file, byte, path, output, or record bound violation.
	ErrLimit = errors.New("ingestion: configured limit exceeded")
	// ErrConflict identifies reuse of an idempotency key for different input.
	ErrConflict = errors.New("ingestion: idempotency conflict")
)

// SymlinkPolicy identifies the only symlink behavior admitted by bounded v1.
type SymlinkPolicy string

const (
	// RecordWithoutFollow records a Git symlink's blob without resolving its target.
	RecordWithoutFollow SymlinkPolicy = "record_without_follow"
)

// Policy selects committed root ignore files and no-follow symlink behavior.
type Policy struct {
	UseGitIgnore       bool          `json:"use_gitignore"`
	UseOuroborosIgnore bool          `json:"use_ouroborosignore"`
	Symlinks           SymlinkPolicy `json:"symlinks"`
}

// Config binds an Authority to one owner-approved repository root.
//
// ApprovedRoot and GitExecutable must be absolute. All limits and the command
// timeout must be positive, making tree enumeration and object reads bounded.
// Construction performs Git I/O but never reads working-tree file contents.
type Config struct {
	ApprovedRoot          string
	GitExecutable         string
	TenantID              string
	BrainID               string
	RepositoryID          string
	ConfigurationDigest   string
	Policy                Policy
	CommandTimeout        time.Duration
	MaxFiles              int
	MaxPathBytes          int
	MaxFileBytes          int64
	MaxTotalBytes         int64
	MaxIdempotencyRecords int
}

// Admission requests the initial committed snapshot.
type Admission struct {
	ExpectedCommitOID string
	IdempotencyKey    string
}

// ReconcileRequest requests an atomic transition between source snapshots.
// ExpectedGenerationID and ExpectedCommitOID form the stale-writer guard.
type ReconcileRequest struct {
	ExpectedGenerationID string
	ExpectedCommitOID    string
	TargetCommitOID      string
	IdempotencyKey       string
}

// HydrationRequest fences and bounds one current-generation content read.
// Limits must be positive and no greater than the Authority configuration.
type HydrationRequest struct {
	ExpectedGenerationID string
	MaxFiles             int
	MaxTotalBytes        int64
}

// RevokeRequest requests immediate deny for the current generation.
type RevokeRequest struct {
	ExpectedGenerationID string
	IdempotencyKey       string
}

// TombstoneRequest requests deletion of transient path-bearing source metadata.
// The source must already be revoked.
type TombstoneRequest struct {
	ExpectedGenerationID string
	IdempotencyKey       string
}

// HintKind describes an untrusted watcher notification.
type HintKind string

const (
	// HintAdd indicates a possibly added path.
	HintAdd HintKind = "add"
	// HintModify indicates a possibly modified path.
	HintModify HintKind = "modify"
	// HintRemove indicates a possibly removed path but never proves deletion.
	HintRemove HintKind = "remove"
	// HintRename indicates a possible old-to-new path transition.
	HintRename HintKind = "rename"
	// HintOverflow marks watcher coverage uncertain and requests full reconcile.
	HintOverflow HintKind = "overflow"
)

// WatchHint is a bounded acceleration signal, never snapshot authority.
type WatchHint struct {
	Kind    HintKind
	Path    string
	OldPath string
}

// EntryKind identifies a committed Git tree entry.
type EntryKind string

const (
	// EntryFile identifies an ordinary committed blob.
	EntryFile EntryKind = "file"
	// EntrySymlink identifies a committed symlink blob recorded without following.
	EntrySymlink EntryKind = "symlink"
)

// FileRevision contains deterministic metadata for one included committed blob.
// Path is repository-relative and must not be stored outside transient projections.
type FileRevision struct {
	Path          string    `json:"path"`
	PathDigest    string    `json:"path_digest"`
	Kind          EntryKind `json:"kind"`
	Mode          string    `json:"mode"`
	SizeBytes     int64     `json:"size_bytes"`
	BlobOID       string    `json:"blob_oid"`
	ContentDigest string    `json:"content_digest"`
	RevisionID    string    `json:"revision_id"`
}

// HydratedFile pairs immutable revision metadata with exact committed bytes.
// Callers own both the returned record and Content slice.
type HydratedFile struct {
	Revision FileRevision
	Content  []byte
}

// ChangeKind identifies one deterministic source delta record.
type ChangeKind string

const (
	// ChangeAdd creates one path.
	ChangeAdd ChangeKind = "add"
	// ChangeModify replaces the revision at one path.
	ChangeModify ChangeKind = "modify"
	// ChangeRename moves an exact blob and mode between paths.
	ChangeRename ChangeKind = "rename"
	// ChangeDelete removes one path after a complete tree proves absence.
	ChangeDelete ChangeKind = "delete"
)

// Change is one source delta record; rename counts as one old/new record.
type Change struct {
	Kind    ChangeKind `json:"kind"`
	OldPath string     `json:"old_path,omitempty"`
	NewPath string     `json:"new_path,omitempty"`
	OldID   string     `json:"old_revision_id,omitempty"`
	NewID   string     `json:"new_revision_id,omitempty"`
}

// Manifest is a complete, deterministic projection of one committed Git tree.
// Digest follows the frozen Stage 3 snapshot-manifest identity contract.
type Manifest struct {
	Digest       string         `json:"digest"`
	PolicyDigest string         `json:"policy_digest"`
	Files        []FileRevision `json:"files"`
}

// Generation is an immutable, atomic source-snapshot publication unit. It does
// not represent product-level readiness or promotion across downstream lanes.
type Generation struct {
	ID                 string   `json:"id"`
	Sequence           uint64   `json:"sequence"`
	SourceID           string   `json:"source_id"`
	SnapshotID         string   `json:"snapshot_id"`
	CommitOID          string   `json:"commit_oid"`
	TreeOID            string   `json:"tree_oid"`
	Manifest           Manifest `json:"manifest"`
	Delta              []Change `json:"delta"`
	ExpectedPreviousID string   `json:"expected_previous_id,omitempty"`
}

// Status reports non-path lifecycle and watcher coverage metadata.
type Status struct {
	SourceID            string
	ApprovedRootID      string
	CurrentGenerationID string
	CurrentCommitOID    string
	Sequence            uint64
	PendingWatcherHints int
	WatcherCoverageLost bool
	Revoked             bool
	Tombstoned          bool
}
