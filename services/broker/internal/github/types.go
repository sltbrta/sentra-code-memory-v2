package github

import (
	"time"

	"github.com/sltbrta/sentra-code-memory-v2/services/internal/contracts"
)

// Effect action vocabulary. Only branch publication and draft-PR creation are
// expressible; merge/deploy/release/force-push/delete are absent by design.
const (
	// ActionBranchPublish is phase 1 head-ref publication.
	ActionBranchPublish = "github.branch.publish"
	// ActionDraftPRCreate is phase 2 draft PR create/reconcile.
	ActionDraftPRCreate = "github.draft_pr.create"
)

// ForbiddenActions are never granted or executed by this broker.
var ForbiddenActions = []string{
	"github.merge",
	"github.deploy",
	"deploy.release",
	"github.release",
	"github.force_push",
	"github.branch.delete",
	"profile.promote",
	"merge",
	"deploy",
}

// OutboxState is the durable two-phase publication progress.
type OutboxState string

const (
	StateBranchPending    OutboxState = "branch_pending"
	StateBranchInFlight   OutboxState = "branch_in_flight"
	StateBranchPublished  OutboxState = "branch_published"
	StatePRPending        OutboxState = "pr_pending"
	StatePRInFlight       OutboxState = "pr_in_flight"
	StatePRCreated        OutboxState = "pr_created"
	StateTerminalConflict OutboxState = "terminal_conflict"
	StatePRAlreadyClosed  OutboxState = "pr_already_closed"
)

// Phase mirrors tracer DraftPrPhase.
type Phase string

const (
	// PhaseBranch is phase 1.
	PhaseBranch Phase = "branch"
	// PhasePR is phase 2.
	PhasePR Phase = "pr"
)

// PublicationTuple is the immutable publication identity. Changing any field
// yields a new effect; neither phase updates a mismatched ref.
type PublicationTuple struct {
	// TenantID scopes the effect.
	TenantID string
	// InstallationID is optional (GitHub App); empty for fine-grained PAT.
	InstallationID string
	// RepositoryOwner and RepositoryName form owner/name.
	RepositoryOwner string
	RepositoryName  string
	// BaseRef is the destination branch under repository policy.
	BaseRef string
	// BaseCommitOID is the approved base commit.
	BaseCommitOID string
	// HeadCommitOID is the candidate head commit.
	HeadCommitOID string
	// ChangeSetDigest binds the validated candidate.
	ChangeSetDigest contracts.Digest
	// EffectApprovalDigest binds the separate current EffectApproval.
	EffectApprovalDigest contracts.Digest
	// PolicyDigest pins current policy.
	PolicyDigest contracts.Digest
	// ConfigDigest pins non-secret configuration.
	ConfigDigest contracts.Digest
}

// PRContent is the deterministic title/body projection (no raw traces/secrets).
type PRContent struct {
	// Title is the draft PR title.
	Title string
	// Body is the draft PR body markdown.
	Body string
}

// EffectGrant is the attenuated draft-only grant rechecked immediately before
// each provider call. It never carries merge/deploy authority.
type EffectGrant struct {
	// GrantID is the opaque grant identity.
	GrantID contracts.Identifier
	// Initiator is the authenticated principal.
	Initiator contracts.Identifier
	// Tenant scopes the grant.
	Tenant contracts.Identifier
	// Actions must be a subset of {github.branch.publish, github.draft_pr.create}.
	Actions []string
	// RepositoryFullName is owner/name.
	RepositoryFullName string
	// BaseCommitOID pins the exact base.
	BaseCommitOID string
	// HeadCommitOID pins the exact head.
	HeadCommitOID string
	// RevocationEpoch is the deny-overlay epoch observed at issuance.
	RevocationEpoch uint64
	// ExpiresAt is the grant expiry wall clock.
	ExpiresAt time.Time
	// PolicyDigest pins the evaluated policy.
	PolicyDigest contracts.Digest
	// Nonce binds the grant instance.
	Nonce string
}

// PublishRequest is one two-phase publication request.
type PublishRequest struct {
	// Authenticated is the trusted mapped identity.
	Authenticated contracts.MappedIdentityFact
	// Tuple is the immutable publication identity.
	Tuple PublicationTuple
	// Content is the deterministic title/body.
	Content PRContent
	// Grant is the current draft-only effect grant.
	Grant EffectGrant
	// IdempotencyKey distinguishes exact retries from conflicts.
	IdempotencyKey string
	// ActionID is the opaque draft-PR action identity.
	ActionID string
}

// Receipt is the two-phase draft-PR outcome (draft-only; no merge/deploy).
type Receipt struct {
	// ActionID is the opaque action identity.
	ActionID string
	// Phase is branch or pr.
	Phase Phase
	// HeadRef is refs/heads/ouroboros/tracer-001/<24hex>.
	HeadRef string
	// BaseRef is the destination branch.
	BaseRef string
	// BaseCommitOID and HeadCommitOID pin exact OIDs.
	BaseCommitOID string
	HeadCommitOID string
	// RepositoryFullName is owner/name.
	RepositoryFullName string
	// ProviderPRID is set once the draft PR exists.
	ProviderPRID string
	// IsDraft is always true on success.
	IsDraft bool
	// PublicationTupleDigest binds the immutable tuple.
	PublicationTupleDigest contracts.Digest
	// ContentDigest binds title/body.
	ContentDigest contracts.Digest
	// EffectApprovalDigest binds the approval.
	EffectApprovalDigest contracts.Digest
	// ChangeSetDigest binds the candidate.
	ChangeSetDigest contracts.Digest
	// OutboxState is the durable progress state.
	OutboxState OutboxState
	// Receipt is the non-sensitive broker receipt.
	Receipt contracts.Receipt
}

// TokenEnvNames lists accepted fine-grained PAT environment variable names.
var TokenEnvNames = []string{"GITHUB_TOKEN", "OUROBOROS_GITHUB_TOKEN"}
