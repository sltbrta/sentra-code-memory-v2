package factory

import (
	"context"

	contractsv1 "github.com/sltbrta/sentra-code-memory-v2/packages/contracts/gen/go/ouroboros/contracts/v1"
	"github.com/sltbrta/sentra-code-memory-v2/services/internal/contracts"
)

// Identity is the authenticated gateway-mapped scope every kernel operation
// runs under. It is always trusted: it arrives from the Unix-peered session,
// never from a request body.
type Identity struct {
	// Tenant, Principal, and Session name the authenticated session scope.
	Tenant    string
	Principal string
	Session   string
}

// CallerCrossCheck is the untrusted body identity carried by a request. The
// kernel compares it against the authenticated Identity and denies on any
// mismatch; it never grants authority.
type CallerCrossCheck struct {
	// Tenant, Principal, and Session must equal the authenticated scope.
	Tenant    string
	Principal string
	Session   string
}

// BaseResolver resolves the exact current committed Git base of the approved
// repository from the Stage 03 ingestion authority. Implementations must fail
// closed; a resolved base is a 40- or 64-character lowercase hex Git object ID.
type BaseResolver interface {
	// CurrentBase returns the exact current base commit for the authenticated
	// scope's approved repository.
	CurrentBase(ctx context.Context, identity Identity) (string, error)
}

// PayloadStore is the narrow encrypted-payload port prose and large canonical
// bytes persist behind, mirroring the Stage 04 conversation payload contract.
// SQLite never holds payload bytes — only the returned opaque artifact identity
// and the canonical SHA-256 digest. Implementations must scope payloads by
// tenant and make reads fail closed.
type PayloadStore interface {
	// Put encrypts and publishes one immutable payload, returning its opaque
	// artifact identity.
	Put(ctx context.Context, tenant string, payload []byte) (artifactID string, err error)
	// Get returns the authenticated plaintext of one published payload.
	Get(ctx context.Context, tenant, artifactID string) (payload []byte, err error)
}

// RouteRequest carries the deterministic facts one leaf routing decision binds.
type RouteRequest struct {
	// RunID and NodeID identify the leaf being routed.
	RunID  string
	NodeID string
	// GoalDigestHex pins the exact leaf goal bytes.
	GoalDigestHex string
	// OwnedPaths is the leaf's exclusive write scope.
	OwnedPaths []string
}

// RouteDecision is the provider-neutral routing outcome recorded on the leaf.
type RouteDecision struct {
	// ProfileDigestHex pins the certified runner/model/profile selection policy.
	ProfileDigestHex string
	// ModelIdentity names the selected provider-neutral model identity.
	ModelIdentity string
	// RationaleCode is a stable non-sensitive routing rationale under policy.
	RationaleCode string
}

// Router derives deterministic model routing facts for plan leaves. The same
// request must always produce the same decision; tests pin a StaticRouter.
type Router interface {
	// Route returns the deterministic routing decision for one leaf.
	Route(ctx context.Context, request RouteRequest) (RouteDecision, error)
}

// Config binds the kernel's dependencies. Every field is required except
// RevocationEpoch, which zero-values to the pre-deny epoch.
type Config struct {
	// DatabasePath is the absolute path of the migrated authority database;
	// migration 005 must already be applied (the composing local authority owns
	// migrations and the process owner lock, so Open takes neither).
	DatabasePath string
	// Payloads stores prose and large canonical bytes in the encrypted vault.
	Payloads PayloadStore
	// Clock supplies wall-clock instants without time.Now dependencies.
	Clock contracts.Clock
	// Bases resolves the exact current Git base for admission revalidation.
	Bases BaseResolver
	// Policy evaluates current policy; admission is authorization-first and a
	// nil or denying checker fails closed.
	Policy contracts.PolicyCheck
	// Router derives deterministic leaf routing facts.
	Router Router
	// LeaseTTLMillis bounds leaf lease lifetime from issuance.
	LeaseTTLMillis int64
	// RevocationEpoch is pinned onto issued leaf grants from the current
	// deny-overlay epoch observed by the composer.
	RevocationEpoch uint64
	// PolicyDigestHex pins the policy evaluated during grant issuance.
	PolicyDigestHex string
}

// AdmitRequest carries one approved intent admission.
type AdmitRequest struct {
	// Authenticated is the trusted session scope.
	Authenticated Identity
	// Caller is the untrusted body identity cross-check.
	Caller CallerCrossCheck
	// Intent is the approved change intent pinned to an exact Git base. Its
	// approval must be present, receipt-completed, and unexpired; its
	// RequestedBy identity must match the authenticated principal.
	Intent *contractsv1.ChangeIntent
	// ApprovedScopePaths is the resolved approved path scope behind the
	// intent's scope digest; every leaf scope must attenuate it.
	ApprovedScopePaths []string
	// Leaves proposes the one-to-three leaf decomposition; the kernel
	// validates prefix-disjoint in-scope scopes and compiles the typed DAG.
	Leaves []LeafSpec
	// Review includes the fresh read-only review node when set.
	Review bool
	// IdempotencyKey distinguishes exact retries from conflicts.
	IdempotencyKey string
}

// LeafSpec proposes one leaf of the compiled DAG.
type LeafSpec struct {
	// NodeID is stable within the plan and matches the frozen node identity
	// pattern: ^[a-z][a-z0-9-]{0,63}$.
	NodeID string
	// Goal is the exact leaf goal prose; it persists in the encrypted vault and
	// the plan carries only its digest.
	Goal []byte
	// OwnedPaths is the leaf's exclusive write scope; scopes are pairwise
	// prefix-disjoint and each must attenuate the intent's approved scope.
	OwnedPaths []string
	// ForbiddenPaths lists explicit protected non-goal write boundaries.
	ForbiddenPaths []string
	// HolderPrincipal names the worker principal the initial lease is issued
	// to; empty derives a deterministic per-node worker identity.
	HolderPrincipal string
}

// AdmitResult returns the opened run.
type AdmitResult struct {
	// RunID is the server-authored admitted run identity.
	RunID string
	// Replayed reports an exact idempotent replay of the original outcome.
	Replayed bool
}

// CancelRequest revokes one admitted run at a safe point.
type CancelRequest struct {
	// Authenticated is the trusted session scope.
	Authenticated Identity
	// Caller is the untrusted body identity cross-check.
	Caller CallerCrossCheck
	// RunID selects the admitted run.
	RunID string
	// IdempotencyKey distinguishes exact retries from conflicts.
	IdempotencyKey string
}

// CancelResult confirms the terminal cancelled state.
type CancelResult struct {
	// RunID echoes the cancelled run identity.
	RunID string
	// Replayed reports an exact idempotent replay of the original outcome.
	Replayed bool
}
