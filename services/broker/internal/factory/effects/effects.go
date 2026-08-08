// Package effects implements the Stage 05 current-policy effect broker.
// Every brokered effect reauthorizes the current identity, grant, policy,
// lease, fence, and idempotency immediately before execution; the UI or
// runner request is never authority. Escape attempts — path traversal outside
// the owned scope, forbidden paths, dispatch or task-creation, and effect
// kinds beyond the sealed surface — deny and fail the run closed.
//
// The grant and lease shapes mirror the frozen CapabilityGrant and Lease
// contract messages. Leaf grants never carry factory.dispatch or
// factory.task.create; a grant that does is malformed and denies everything.
package effects

import (
	"context"
	"errors"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/sltbrta/sentra-code-memory-v2/services/broker/internal/factory/changeset"
	"github.com/sltbrta/sentra-code-memory-v2/services/broker/internal/factory/gitcandidate"
	"github.com/sltbrta/sentra-code-memory-v2/services/internal/contracts"
)

// ErrDenied is the single non-disclosing effect denial.
var ErrDenied = errors.New("effects: denied")

// Bounded effect action vocabulary. Only file effects are executable on the
// v1 sealed runner surface; every other kind denies as an escape attempt.
const (
	ActionFileRead  = "file.read"
	ActionFileWrite = "file.write"

	// ActionDispatch and ActionTaskCreate are never leaf authority. The
	// frozen contract makes them inexpressible in a leaf grant; requesting
	// them is an escape attempt that fails the run closed.
	ActionDispatch   = "factory.dispatch"
	ActionTaskCreate = "factory.task.create"
)

// Internal trace reason codes. Public edges collapse all of these to
// not_found_or_denied; the codes never carry request detail.
const (
	ReasonIdentityMismatch    = "identity_mismatch"
	ReasonGrantMalformed      = "grant_malformed"
	ReasonGrantExpired        = "grant_expired"
	ReasonBaseMismatch        = "base_mismatch"
	ReasonEscapePathScope     = "escape_path_scope"
	ReasonEscapeForbiddenPath = "escape_forbidden_path"
	ReasonEscapePathShape     = "escape_path_shape"
	ReasonEscapeDispatch      = "escape_dispatch"
	ReasonEscapeEffectKind    = "escape_effect_kind"
	ReasonStaleLease          = "stale_lease"
	ReasonStaleFence          = "stale_fence"
	ReasonStaleEpoch          = "stale_epoch"
	ReasonPolicyDenied        = "policy_denied"
	ReasonIdempotencyConflict = "idempotency_conflict"
	ReasonInvalidEdit         = "invalid_edit"
	ReasonAllowed             = "allowed"
)

// Denial is a typed effect denial carrying one static internal reason code.
type Denial struct {
	Reason string
}

// Error implements error.
func (d *Denial) Error() string { return "effects: denied: " + d.Reason }

// Unwrap exposes ErrDenied for errors.Is.
func (d *Denial) Unwrap() error { return ErrDenied }

// ReasonCode extracts the static internal reason from one denial error.
func ReasonCode(err error) string {
	var denial *Denial
	if errors.As(err, &denial) {
		return denial.Reason
	}
	return "denied"
}

// IsEscape reports whether one denial is an escape attempt, which fails the
// whole run closed rather than merely rejecting one effect.
func IsEscape(err error) bool {
	return strings.HasPrefix(ReasonCode(err), "escape_")
}

// Lease mirrors the frozen Lease contract message.
type Lease struct {
	LeaseID   contracts.Identifier
	Holder    contracts.Identifier
	Fence     uint64
	ExpiresAt time.Time
}

// Grant mirrors the frozen CapabilityGrant contract message: the exact
// attenuated leaf execution authority pinned to one exact Git base.
type Grant struct {
	GrantID          contracts.Identifier
	Initiator        contracts.Identifier
	Tenant           contracts.Identifier
	TaskID           contracts.Identifier
	RunID            contracts.Identifier
	Lease            Lease
	Actions          []string
	Resources        []contracts.Identifier
	RepositoryGitOID string
	AllowedPaths     []string
	Nonce            string
	RevocationEpoch  uint64
	ExpiresAt        time.Time
	PolicyDigest     contracts.Digest
	CommandFence     uint64
}

// Scope carries the plan-node write boundary the broker enforces on paths.
type Scope struct {
	OwnedPaths     []string
	ForbiddenPaths []string
	BaseGitOID     string
}

// Leaf binds one leased leaf identity, grant, and scope for authorization.
type Leaf struct {
	Identity contracts.MappedIdentityFact
	Grant    Grant
	Scope    Scope
}

// Request is one bounded effect request. Path is required for file actions
// and empty otherwise; the action vocabulary is exact and wildcard-free.
// Now is an optional caller fact; the broker never evaluates it for
// authorization — every temporal check uses the broker's live clock.
type Request struct {
	Action         string
	Path           string
	Resource       contracts.Identifier
	IdempotencyKey string
	Now            time.Time
}

// FenceRegistry resolves the current lease fence state. The kernel owns the
// canonical registry; the broker queries it at every mutation.
type FenceRegistry interface {
	CurrentFence(ctx context.Context, leaseID contracts.Identifier) (fence uint64, expiresAt time.Time, ok bool)
}

// Broker authorizes and idempotently executes effects under current policy.
// The clock supplies the current instant for every reauthorization so lease,
// fence, and grant expiry are evaluated at mutation time.
type Broker struct {
	policy   contracts.PolicyCheck
	fences   FenceRegistry
	clock    func() time.Time
	mu       sync.Mutex
	executed map[string]executedEffect
}

type executedEffect struct {
	requestDigest contracts.Digest
	receipt       contracts.Receipt
}

// NewBroker returns a default-deny effect broker over the current-policy
// check port, the lease fence registry, and the current-time clock. All
// three are required.
func NewBroker(policy contracts.PolicyCheck, fences FenceRegistry, clock func() time.Time) (*Broker, error) {
	if policy == nil || fences == nil || clock == nil {
		return nil, ErrDenied
	}
	return &Broker{policy: policy, fences: fences, clock: clock, executed: make(map[string]executedEffect)}, nil
}

// ValidateGrant rejects malformed, wildcarded, expired, or dispatch-carrying
// leaf grant authority. Trusted issuance runs it before a grant resolves.
func ValidateGrant(grant Grant, now time.Time) error {
	if grant.GrantID.Namespace != "grant" || grant.GrantID.Value == "" ||
		grant.Initiator.Namespace != "principal" || grant.Initiator.Value == "" ||
		grant.Tenant.Namespace != "tenant" || grant.Tenant.Value == "" ||
		grant.Nonce == "" || grant.CommandFence == 0 || grant.ExpiresAt.IsZero() ||
		!now.Before(grant.ExpiresAt) || grant.PolicyDigest.Algorithm != "sha256" ||
		len(grant.PolicyDigest.Hex) != 64 || len(grant.Actions) == 0 || len(grant.Resources) == 0 {
		return &Denial{Reason: ReasonGrantMalformed}
	}
	if grant.Lease.LeaseID.Namespace != "lease" || grant.Lease.LeaseID.Value == "" ||
		grant.Lease.Holder != grant.Initiator || grant.Lease.Fence == 0 ||
		grant.Lease.ExpiresAt.IsZero() || !now.Before(grant.Lease.ExpiresAt) {
		return &Denial{Reason: ReasonGrantMalformed}
	}
	if !validGitOID(grant.RepositoryGitOID) {
		return &Denial{Reason: ReasonGrantMalformed}
	}
	for _, action := range grant.Actions {
		if !validAction(action) || action == ActionDispatch || action == ActionTaskCreate {
			return &Denial{Reason: ReasonGrantMalformed}
		}
	}
	seen := make(map[string]struct{}, len(grant.Actions))
	for _, action := range grant.Actions {
		if _, duplicate := seen[action]; duplicate {
			return &Denial{Reason: ReasonGrantMalformed}
		}
		seen[action] = struct{}{}
	}
	for _, resource := range grant.Resources {
		if resource.Namespace == "" || resource.Value == "" || strings.Contains(resource.Value, "*") {
			return &Denial{Reason: ReasonGrantMalformed}
		}
	}
	for _, prefix := range grant.AllowedPaths {
		if changeset.ValidatePath(prefix) != nil {
			return &Denial{Reason: ReasonGrantMalformed}
		}
	}
	return nil
}

// Authorize performs the complete current-policy check for one effect
// request without executing it. Every dimension is re-evaluated: identity,
// grant validity and expiry, base binding, action and path scope, lease and
// fence currency, revocation epoch, and current policy.
func (b *Broker) Authorize(ctx context.Context, leaf Leaf, request Request) error {
	if b == nil || ctx == nil {
		return &Denial{Reason: ReasonPolicyDenied}
	}
	if err := b.authorizeLeaf(ctx, leaf, request); err != nil {
		return err
	}
	return nil
}

// Execute authorizes one effect under current policy and runs its mutation at
// most once per idempotency key. An exact replay re-authorizes against
// current policy first and then returns the original receipt without
// re-executing; a conflicting key reuse denies. The mutation runs only after
// every current check passes. An in-flight placeholder under the broker lock
// makes exactly-once hold for concurrent same-key calls: the second caller is
// rejected rather than serialized, and a failed mutation stays claimed so a
// retry cannot double-execute.
func (b *Broker) Execute(ctx context.Context, leaf Leaf, request Request, mutate func(context.Context) error) (contracts.Receipt, error) {
	receipt := contracts.Receipt{
		OperationID: contracts.Identifier{Namespace: "effect", Value: request.IdempotencyKey},
		Status:      "rejected",
	}
	if b == nil || ctx == nil || mutate == nil || request.IdempotencyKey == "" {
		receipt.ReasonCode = ReasonPolicyDenied
		return receipt, &Denial{Reason: ReasonPolicyDenied}
	}
	if err := b.authorizeLeaf(ctx, leaf, request); err != nil {
		receipt.ReasonCode = ReasonCode(err)
		return receipt, err
	}
	requestDigest := changeset.DigestBytes([]byte(strings.Join([]string{
		"ouroboros.stage05.effect-request.v1",
		leaf.Grant.GrantID.Value,
		request.Action,
		request.Path,
		request.Resource.Namespace,
		request.Resource.Value,
		request.IdempotencyKey,
	}, "\x00")))
	// Idempotency is namespaced by tenant and grant identity, matching the
	// command-record scope: two grants may reuse one key, while one grant
	// executes a given key at most once.
	namespace := strings.Join([]string{leaf.Grant.Tenant.Value, leaf.Grant.GrantID.Value, request.IdempotencyKey}, "\x00")
	b.mu.Lock()
	recorded, found := b.executed[namespace]
	if !found {
		b.executed[namespace] = executedEffect{requestDigest: requestDigest}
	}
	b.mu.Unlock()
	if found {
		if recorded.requestDigest != requestDigest || recorded.receipt.Status == "" {
			receipt.ReasonCode = ReasonIdempotencyConflict
			return receipt, &Denial{Reason: ReasonIdempotencyConflict}
		}
		return recorded.receipt, nil
	}
	if err := mutate(ctx); err != nil {
		receipt.ReasonCode = ReasonPolicyDenied
		return receipt, err
	}
	receipt.Status = "completed"
	receipt.ReasonCode = ReasonAllowed
	b.mu.Lock()
	b.executed[namespace] = executedEffect{requestDigest: requestDigest, receipt: receipt}
	b.mu.Unlock()
	return receipt, nil
}

// Bind returns the mutation authorizer for one leased leaf. The candidate
// store calls it before every candidate mutation, so lease, fence, and grant
// are reauthorized at mutation time rather than only at admission.
func (b *Broker) Bind(leaf Leaf) *BoundBroker {
	return &BoundBroker{broker: b, leaf: leaf}
}

// BoundBroker authorizes candidate mutations for one bound leaf.
type BoundBroker struct {
	broker *Broker
	leaf   Leaf
}

// AuthorizeMutation reauthorizes the bound leaf for one candidate file
// mutation. A rename consumes its pre-image path, so both paths must be in
// scope; every mutation requires current file.write authority.
func (bb *BoundBroker) AuthorizeMutation(ctx context.Context, mutation gitcandidate.Mutation) error {
	if bb == nil || bb.broker == nil {
		return &Denial{Reason: ReasonPolicyDenied}
	}
	edit := mutation.Edit
	if err := bb.broker.authorizeLeaf(ctx, bb.leaf, Request{Action: ActionFileWrite, Path: edit.Path}); err != nil {
		return err
	}
	if edit.Op == changeset.OpRename {
		if err := bb.broker.authorizeLeaf(ctx, bb.leaf, Request{Action: ActionFileWrite, Path: edit.OldPath}); err != nil {
			return err
		}
	}
	return nil
}

func (b *Broker) authorizeLeaf(ctx context.Context, leaf Leaf, request Request) error {
	// Authorization always evaluates the broker's live clock. A caller-
	// supplied Request.Now is recorded as a fact but never evaluated, so no
	// caller can authorize against an earlier instant after expiry.
	now := b.clock()
	if now.IsZero() {
		return &Denial{Reason: ReasonGrantMalformed}
	}
	if leaf.Identity.Principal != leaf.Grant.Initiator || leaf.Identity.Tenant != leaf.Grant.Tenant {
		return &Denial{Reason: ReasonIdentityMismatch}
	}
	if err := ValidateGrant(leaf.Grant, now); err != nil {
		return err
	}
	if leaf.Scope.BaseGitOID != leaf.Grant.RepositoryGitOID {
		return &Denial{Reason: ReasonBaseMismatch}
	}
	if request.Action == ActionDispatch || request.Action == ActionTaskCreate {
		return &Denial{Reason: ReasonEscapeDispatch}
	}
	if request.Action != ActionFileRead && request.Action != ActionFileWrite {
		return &Denial{Reason: ReasonEscapeEffectKind}
	}
	if !containsAction(leaf.Grant.Actions, request.Action) {
		return &Denial{Reason: ReasonPolicyDenied}
	}
	if err := authorizePath(leaf, request.Path); err != nil {
		return err
	}
	fence, leaseExpiry, ok := b.fences.CurrentFence(ctx, leaf.Grant.Lease.LeaseID)
	if !ok || !now.Before(leaseExpiry) {
		return &Denial{Reason: ReasonStaleLease}
	}
	if fence != leaf.Grant.Lease.Fence {
		return &Denial{Reason: ReasonStaleFence}
	}
	resource := request.Resource
	if resource.Namespace == "" {
		resource = leaf.Grant.Resources[0]
	}
	if !containsResource(leaf.Grant.Resources, resource) {
		return &Denial{Reason: ReasonPolicyDenied}
	}
	decision, err := b.policy.Check(ctx, leaf.Identity, contracts.PolicyRequest{
		Action:          request.Action,
		Resource:        resource,
		RevocationEpoch: leaf.Grant.RevocationEpoch,
	})
	if err != nil || !decision.Allowed {
		return &Denial{Reason: ReasonPolicyDenied}
	}
	if decision.RevocationEpoch != leaf.Grant.RevocationEpoch {
		return &Denial{Reason: ReasonStaleEpoch}
	}
	return nil
}

// authorizePath enforces the plan-node write boundary on one file path: the
// normalized path must sit inside both the leaf owned scope and the grant
// allowed paths, and never inside a forbidden path.
func authorizePath(leaf Leaf, requestPath string) error {
	if changeset.ValidatePath(requestPath) != nil || path.IsAbs(requestPath) {
		return &Denial{Reason: ReasonEscapePathShape}
	}
	if withinAny(requestPath, leaf.Scope.ForbiddenPaths) {
		return &Denial{Reason: ReasonEscapeForbiddenPath}
	}
	if !withinAny(requestPath, leaf.Scope.OwnedPaths) || !withinAny(requestPath, leaf.Grant.AllowedPaths) {
		return &Denial{Reason: ReasonEscapePathScope}
	}
	return nil
}

// withinAny reports whether value equals one prefix or sits beneath it.
func withinAny(value string, prefixes []string) bool {
	for _, prefix := range prefixes {
		if value == prefix || strings.HasPrefix(value, prefix+"/") {
			return true
		}
	}
	return false
}

func validAction(action string) bool {
	if action == "" || len(action) > 128 || strings.Contains(action, "*") {
		return false
	}
	for _, character := range action {
		if character >= 'a' && character <= 'z' || character >= '0' && character <= '9' ||
			character == '.' || character == '_' || character == ':' || character == '-' {
			continue
		}
		return false
	}
	return action[0] >= 'a' && action[0] <= 'z'
}

// validGitOID enforces the exact lowercase-hex Git object identifier shape
// pinned by the frozen grant and intent contracts.
func validGitOID(value string) bool {
	if len(value) != 40 && len(value) != 64 {
		return false
	}
	for _, character := range value {
		if character >= '0' && character <= '9' || character >= 'a' && character <= 'f' {
			continue
		}
		return false
	}
	return true
}

func containsAction(actions []string, want string) bool {
	for _, action := range actions {
		if action == want {
			return true
		}
	}
	return false
}

func containsResource(resources []contracts.Identifier, want contracts.Identifier) bool {
	for _, resource := range resources {
		if resource == want {
			return true
		}
	}
	return false
}
