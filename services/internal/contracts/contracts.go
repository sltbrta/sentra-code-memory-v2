// Package contracts defines implementation-free Stage 02 authority ports.
// Runtime packages depend inward on these types so policy, ledger, and ArtifactVault
// implementations can change without sibling-internal imports or ambient context state.
package contracts

import "context"

// Identifier is a namespaced opaque identity that must not cross authority domains.
type Identifier struct {
	// Namespace distinguishes identity domains such as tenant, artifact, or command.
	Namespace string
	// Value is an opaque non-empty value validated at the outer boundary.
	Value string
}

// Digest binds immutable data to a named algorithm and encoded value.
type Digest struct {
	// Algorithm names the digest function.
	Algorithm string
	// Hex is the canonical encoded digest value.
	Hex string
}

// Receipt is a static public disposition; it never carries upstream error text.
type Receipt struct {
	// OperationID identifies the command or operation that produced this result.
	OperationID Identifier
	// Status is a finite canonical outcome selected by the ledger.
	Status string
	// ReasonCode is a stable non-sensitive reason for clients and audit.
	ReasonCode string
	// Watermark is the canonical position observed while recording the receipt.
	Watermark uint64
}

// Clock supplies wall-clock values without making domain logic depend on time.Now.
type Clock interface {
	// NowUnixMilli returns the current wall-clock instant in milliseconds.
	NowUnixMilli() int64
}

// PeerCredentials are raw operating-system facts captured before request decoding.
type PeerCredentials struct {
	// UID is the operating-system user identifier presented by the peer.
	UID uint32
	// GID is the operating-system group identifier presented by the peer.
	GID uint32
	// PID is the operating-system process identifier presented by the peer.
	PID uint32
}

// MappedIdentityFact is the trusted mapping from raw peer credentials to authority identity.
type MappedIdentityFact struct {
	// Principal is the mapped principal identity.
	Principal Identifier
	// Tenant is the mapped tenant identity.
	Tenant Identifier
	// Session is the mapped authenticated local session identity.
	Session Identifier
	// Credentials are the raw peer facts that were mapped.
	Credentials PeerCredentials
}

// PolicyRequest names the exact action and tenant-scoped resource under review.
type PolicyRequest struct {
	// Action is a stable, typed authority action.
	Action string
	// Resource is the exact tenant-scoped resource under review.
	Resource Identifier
	// RevocationEpoch is the caller's observed deny-overlay epoch.
	RevocationEpoch uint64
}

// PolicyDecision records a fail-closed current-policy outcome.
type PolicyDecision struct {
	// Allowed is false for missing, stale, malformed, or denied policy facts.
	Allowed bool
	// Receipt records the non-sensitive decision evidence.
	Receipt Receipt
	// RevocationEpoch is the epoch evaluated by the policy implementation.
	RevocationEpoch uint64
}

// PolicyCheck evaluates current policy and must default deny on missing facts.
type PolicyCheck interface {
	// Check returns a current decision; implementations must not authorize from body identity.
	Check(context.Context, MappedIdentityFact, PolicyRequest) (PolicyDecision, error)
}

// KeyReference identifies a root/key epoch while keeping secret material opaque.
type KeyReference struct {
	// Root identifies the encryption-root domain.
	Root Identifier
	// KeyID identifies the current, historical, or legacy key reference.
	KeyID Identifier
	// Epoch selects the key history entry.
	Epoch uint64
	// Legacy marks a read requiring legacy migration handling.
	Legacy bool
}

// KeyRoot resolves current and historical key references without exposing key bytes.
type KeyRoot interface {
	// Resolve returns an opaque reference or a typed error; it never returns secret material.
	Resolve(context.Context, Identifier, uint64) (KeyReference, error)
}

// CommandRecord is the canonical command metadata written atomically with its receipt.
type CommandRecord struct {
	// Command is the opaque command identity.
	Command Identifier
	// Tenant scopes idempotency and authority.
	Tenant Identifier
	// Principal is the authenticated actor used in idempotency scope.
	Principal Identifier
	// Session scopes the authenticated caller.
	Session Identifier
	// CommandType distinguishes independently retryable operation classes.
	CommandType string
	// IdempotencyKey distinguishes exact retries from conflicts.
	IdempotencyKey string
	// AuthenticatedDigest binds the exact authenticated request.
	AuthenticatedDigest Digest
	// Fence binds admission to the caller's current command-scope authority.
	Fence uint64
}

// EventRecord is canonical immutable event metadata without an implementation payload.
type EventRecord struct {
	// Event identifies the immutable event.
	Event Identifier
	// Aggregate identifies the versioned aggregate.
	Aggregate Identifier
	// Version is the next aggregate version to record.
	Version uint64
	// PayloadDigest binds separately stored immutable payload bytes.
	PayloadDigest Digest
}

// WatermarkRecord identifies a projection's complete canonical position.
type WatermarkRecord struct {
	// Projection identifies the rebuildable projection.
	Projection string
	// Tenant scopes the projection watermark.
	Tenant Identifier
	// Value is the complete observed canonical position.
	Value uint64
}

// AuditRecord binds an event to the prior audit digest for verification on read.
type AuditRecord struct {
	// Tenant scopes the audit chain.
	Tenant Identifier
	// EventDigest binds the recorded event.
	EventDigest Digest
	// PreviousDigest is empty only at a verified chain origin.
	PreviousDigest Digest
}

// ArtifactManifest identifies one immutable artifact generation and framed layout.
type ArtifactManifest struct {
	// Artifact identifies the immutable artifact.
	Artifact Identifier
	// Tenant scopes the artifact and prevents cross-tenant reuse.
	Tenant Identifier
	// Digest binds immutable content.
	Digest Digest
	// Generation is the immutable artifact generation.
	Generation uint64
	// KeyEpoch identifies the encryption-key history entry.
	KeyEpoch uint64
	// Length is the total bounded artifact byte length.
	Length uint64
	// FrameCount is the bounded independently authenticated frame count.
	FrameCount uint32
}

// LedgerTx is the explicit narrow transaction capability shared by authority leaves.
type LedgerTx interface {
	// PutCommand records command/idempotency metadata or returns a conflict.
	PutCommand(context.Context, CommandRecord) error
	// AppendEvent records one aggregate-versioned event and returns its canonical sequence.
	AppendEvent(context.Context, EventRecord) (uint64, error)
	// PutReceipt records a static receipt atomically with the command and event.
	PutReceipt(context.Context, Receipt) error
	// AdvanceWatermark records a complete monotonic projection position.
	AdvanceWatermark(context.Context, WatermarkRecord) error
	// AppendAudit records hash-linked audit metadata.
	AppendAudit(context.Context, AuditRecord) error
	// PutManifest records immutable artifact metadata without storing artifact bytes.
	PutManifest(context.Context, ArtifactManifest) error
}

// AtomicLedgerTransaction executes a mutation with an explicit transaction capability.
type AtomicLedgerTransaction interface {
	// Within atomically commits callback mutations or rolls all of them back on error.
	Within(context.Context, func(context.Context, LedgerTx) error) error
}

// ByteRange is an inclusive-exclusive bounded artifact byte range.
type ByteRange struct {
	// Offset is the zero-based starting byte.
	Offset uint64
	// Length is the bounded number of bytes requested.
	Length uint64
}

// ArtifactStageRequest describes artifact metadata staged after boundary validation.
type ArtifactStageRequest struct {
	// Manifest is the immutable artifact layout to stage.
	Manifest ArtifactManifest
	// ExpectedGeneration permits zero for creation and otherwise prevents stale staging.
	ExpectedGeneration uint64
}

// ArtifactPublishRequest makes a fully staged immutable manifest current conditionally.
type ArtifactPublishRequest struct {
	// Manifest is the fully staged immutable layout to publish.
	Manifest ArtifactManifest
	// ExpectedGeneration is the required current generation.
	ExpectedGeneration uint64
}

// ArtifactReadRequest bounds a hydrated range to one immutable generation.
type ArtifactReadRequest struct {
	// Artifact identifies the artifact to read.
	Artifact Identifier
	// Tenant scopes the requested artifact.
	Tenant Identifier
	// Generation selects the immutable generation.
	Generation uint64
	// Range bounds hydration before object lookup.
	Range ByteRange
}

// ArtifactReadResult returns authenticated frame references, never an object-store handle.
type ArtifactReadResult struct {
	// Manifest identifies the generation read.
	Manifest ArtifactManifest
	// FrameDigests bind the returned bounded frame sequence.
	FrameDigests []Digest
	// NextOffset continues a bounded range operation.
	NextOffset uint64
}

// ArtifactReconcileRequest asks for safe immutable-manifest reconciliation only.
type ArtifactReconcileRequest struct {
	// Artifact identifies the artifact to reconcile.
	Artifact Identifier
	// Tenant scopes reconciliation.
	Tenant Identifier
	// ObservedGeneration is the remote or local generation under comparison.
	ObservedGeneration uint64
}

// TombstoneRequest applies immediate deny state before physical purge.
type TombstoneRequest struct {
	// Artifact identifies the artifact to deny.
	Artifact Identifier
	// Tenant scopes the tombstone.
	Tenant Identifier
	// ExpectedGeneration prevents deletion of an unintended generation.
	ExpectedGeneration uint64
	// ReasonCode is a static non-sensitive deletion reason.
	ReasonCode string
}

// TombstoneResult records the deletion overlay and resulting receipt.
type TombstoneResult struct {
	// Tombstoned reports whether the deny overlay is canonical.
	Tombstoned bool
	// Receipt records the static deletion outcome.
	Receipt Receipt
}

// PurgeRequest conveys an L1-authorized physical-purge request to L2.
type PurgeRequest struct {
	// Artifact identifies the artifact to physically remove.
	Artifact Identifier
	// Tenant scopes physical deletion.
	Tenant Identifier
	// TombstoneReceipt proves immediate deny was recorded before purge.
	TombstoneReceipt Receipt
	// KeyEpoch selects the key disposition scope.
	KeyEpoch uint64
}

// PurgeResult describes a bounded purge outcome without claiming backup erasure.
type PurgeResult struct {
	// Purged reports primary payload/sidecar disposition only.
	Purged bool
	// Quarantined reports an unrecoverable anomaly requiring later handling.
	Quarantined bool
	// Receipt records the static physical-purge outcome.
	Receipt Receipt
}

// ArtifactVault is the only artifact byte authority; it intentionally has no generic object API.
type ArtifactVault interface {
	// Stage records bounded staged metadata without publishing it as current.
	Stage(context.Context, ArtifactStageRequest) (Receipt, error)
	// Publish conditionally makes a complete staged generation current.
	Publish(context.Context, ArtifactPublishRequest) (Receipt, error)
	// ReadRange returns bounded authenticated frame metadata after authorization.
	ReadRange(context.Context, ArtifactReadRequest) (ArtifactReadResult, error)
	// Reconcile compares immutable metadata and reports a static outcome.
	Reconcile(context.Context, ArtifactReconcileRequest) (Receipt, error)
	// Tombstone applies immediate deny before asynchronous physical deletion.
	Tombstone(context.Context, TombstoneRequest) (TombstoneResult, error)
	// Purge physically disposes scoped primary data after a tombstone.
	Purge(context.Context, PurgeRequest) (PurgeResult, error)
}

// RenderModel is static bounded terminal presentation data, never an upstream error body.
type RenderModel struct {
	// Title is the short terminal heading.
	Title string
	// Detail is a safe bounded explanatory string.
	Detail string
	// ActionLabel is an optional safe next-action label.
	ActionLabel string
}
