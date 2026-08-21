package authorityprocess

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	contractsv1 "github.com/sltbrta/sentra-code-memory-v2/packages/contracts/gen/go/ouroboros/contracts/v1"
	brain "github.com/sltbrta/sentra-code-memory-v2/services/brain/localauthority"
	broker "github.com/sltbrta/sentra-code-memory-v2/services/broker/localauthority"
	"github.com/sltbrta/sentra-code-memory-v2/services/gateway/internal/factoryapi"
	gateway "github.com/sltbrta/sentra-code-memory-v2/services/gateway/internal/localauthority"
	"github.com/sltbrta/sentra-code-memory-v2/services/gateway/internal/localbootstrap"
	shared "github.com/sltbrta/sentra-code-memory-v2/services/internal/contracts"
)

// The Stage 05 factory orchestration adapter: the production factoryapi.Kernel
// implementation that drives the deterministic kernel, the sealed runner, and
// the deterministic gate and review evaluations behind the frozen five-RPC
// surface. Admission revalidates the operator-staged approval descriptor
// against the authorized vault read — never client bytes; candidate execution
// drives lazily on the first candidate read and review on the first findings
// read, so every run fact commits through the kernel with fence, policy, and
// epoch checks and replays deterministically across restarts.

const (
	factoryDefaultLeaseTTLMillis = 60_000
	factoryMinLeaseTTLMillis     = 250
	factoryMaxLeaseTTLMillis     = 600_000
)

// factoryPolicyAdapter is the current-policy port the kernel and the effect
// broker share: every factory action evaluates against the exact tenant-scoped
// brain relationship through the production broker evaluator, and the current
// revocation epoch flows back so stale pinned epochs deny.
type factoryPolicyAdapter struct {
	broker *broker.Broker
	brain  broker.Identifier
}

func (adapter factoryPolicyAdapter) Check(
	ctx context.Context, mapped shared.MappedIdentityFact, request shared.PolicyRequest,
) (shared.PolicyDecision, error) {
	if adapter.broker == nil || ctx == nil {
		return shared.PolicyDecision{}, nil
	}
	// The requested resource decides, not a fixed brain identifier.
	//
	// This discarded request.Resource and always authorised against
	// adapter.brain, which made the broker's containsResource check a
	// tautology: policy was evaluated per-tenant rather than per-resource, so a
	// grant for one resource authorised every resource in the tenant.
	resource := adapter.brain
	if request.Resource.Value != "" {
		resource = broker.Identifier(request.Resource)
	}
	decision, err := adapter.broker.AuthorizeSource(ctx, mapped, request.Action, resource)
	if err != nil {
		return shared.PolicyDecision{RevocationEpoch: decision.RevocationEpoch}, nil
	}
	return shared.PolicyDecision{Allowed: decision.Allowed, RevocationEpoch: decision.RevocationEpoch}, nil
}

// factoryLeaseFact is the current fence and expiry of one issued lease, read
// from the kernel's durable roster through the served plan.
type factoryLeaseFact struct {
	fence     uint64
	expiresAt time.Time
}

// factoryFenceRegistry is the effect broker's fence port: it answers from the
// kernel's durable lease facts, loaded from the served plan before every leaf
// execution so a restarted composition re-derives exactly the kernel state.
type factoryFenceRegistry struct {
	mu     sync.RWMutex
	leases map[string]factoryLeaseFact
}

func newFactoryFenceRegistry() *factoryFenceRegistry {
	return &factoryFenceRegistry{leases: make(map[string]factoryLeaseFact)}
}

func (registry *factoryFenceRegistry) load(plan *contractsv1.ChangePlan) {
	if registry == nil || plan == nil {
		return
	}
	registry.mu.Lock()
	defer registry.mu.Unlock()
	for _, node := range plan.GetNodes() {
		lease := node.GetLease()
		if lease == nil || lease.GetLeaseId().GetValue() == "" {
			continue
		}
		registry.leases[lease.GetLeaseId().GetValue()] = factoryLeaseFact{
			fence:     lease.GetFence(),
			expiresAt: lease.GetExpiresAt().AsTime(),
		}
	}
}

func (registry *factoryFenceRegistry) CurrentFence(
	_ context.Context, leaseID shared.Identifier,
) (uint64, time.Time, bool) {
	if registry == nil {
		return 0, time.Time{}, false
	}
	registry.mu.RLock()
	defer registry.mu.RUnlock()
	fact, found := registry.leases[leaseID.Value]
	if !found {
		return 0, time.Time{}, false
	}
	return fact.fence, fact.expiresAt, true
}

// factoryKernelAdapter is the production factoryapi.Kernel port: the five
// frozen operations over the deterministic kernel, the sealed runner, and the
// deterministic gate and review evaluations.
type factoryKernelAdapter struct {
	kernel    *brain.FactoryKernel
	runner    *broker.FactoryRunner
	runtime   *brain.Runtime
	broker    *broker.Broker
	config    *localbootstrap.Config
	identity  brain.Identity
	keyEpoch  uint64
	now       func() time.Time
	fences    *factoryFenceRegistry
	configHex string
	// toolchain compiles and tests a candidate change set against the real
	// module for the BUILD and TEST gates. A zero value cannot run anything,
	// and those two gates then fail rather than reporting a pass they did not
	// earn -- which is the state the whole surface was in before it existed.
	toolchain factoryToolchain
}

var _ factoryapi.Kernel = (*factoryKernelAdapter)(nil)

// AdmitChangeIntent admits one approved intent: the approval descriptor
// revalidates against the authorized vault read, then the kernel revalidates
// approval, exact base, and evidence under current policy before opening the
// run. An exact idempotent replay returns the original run without
// re-executing; every denial shares the static non-disclosing shape.
func (adapter *factoryKernelAdapter) AdmitChangeIntent(
	ctx context.Context, command factoryapi.AdmitIntentCommand,
) (*contractsv1.AdmitChangeIntentSuccess, error) {
	if adapter.kernel == nil || ctx == nil {
		return nil, factoryapi.ErrUnknownRun
	}
	identity := factoryIdentity(command.Principal)
	descriptor, err := adapter.resolveDescriptor(ctx, command.Intent)
	if err != nil {
		return nil, factoryapi.ErrUnknownRun
	}
	leaves := make([]brain.FactoryLeafSpec, 0, len(descriptor.Leaves))
	for _, leaf := range descriptor.Leaves {
		leaves = append(leaves, brain.FactoryLeafSpec{
			NodeID:         leaf.NodeID,
			Goal:           []byte(leaf.Goal),
			OwnedPaths:     append([]string(nil), leaf.OwnedPaths...),
			ForbiddenPaths: append([]string(nil), leaf.ForbiddenPaths...),
			// The v1 leaf holder is the authenticated owner itself, so the served
			// grant's lease holder equals its initiator exactly as the sealed
			// runner requires; the fresh reviewer stays a distinct principal.
			HolderPrincipal: identity.Principal,
		})
	}
	admitted, err := adapter.kernel.AdmitChangeIntent(ctx, brain.FactoryAdmitRequest{
		Authenticated:      identity,
		Caller:             factoryCaller(identity),
		Intent:             command.Intent,
		ApprovedScopePaths: append([]string(nil), descriptor.ScopePaths...),
		Leaves:             leaves,
		Review:             descriptor.Review,
		IdempotencyKey:     command.IdempotencyKey,
	})
	if err != nil {
		return nil, mapFactoryKernelError(err)
	}
	return &contractsv1.AdmitChangeIntentSuccess{
		RunId: &contractsv1.Identifier{Namespace: "factory-run", Value: admitted.RunID},
		State: contractsv1.ChangeRunState_CHANGE_RUN_STATE_PLANNING,
	}, nil
}

// ChangePlan reads the typed one-layer DAG for one admitted run.
func (adapter *factoryKernelAdapter) ChangePlan(
	ctx context.Context, principal factoryapi.Principal, runID string,
) (*contractsv1.ChangePlan, error) {
	if adapter.kernel == nil || ctx == nil {
		return nil, factoryapi.ErrUnknownRun
	}
	plan, err := adapter.kernel.GetChangePlan(ctx, factoryIdentity(principal), runID)
	if err != nil {
		return nil, mapFactoryKernelError(err)
	}
	return plan, nil
}

// ChangeSetPreview drives candidate execution lazily on the first candidate
// read — leaves execute through the sealed runner, the deterministic gates
// evaluate the candidate facts, and the atomic candidate verifies or rejects
// with its rollback receipt — then serves the canonical preview. Re-driving
// after a crash replays deterministically because every step is either a
// kernel-replayed fact or a deterministic derivation from durable state.
func (adapter *factoryKernelAdapter) ChangeSetPreview(
	ctx context.Context, principal factoryapi.Principal, runID string,
) (*contractsv1.ChangeSetPreview, error) {
	if adapter.kernel == nil || ctx == nil {
		return nil, factoryapi.ErrUnknownRun
	}
	identity := factoryIdentity(principal)
	plan, err := adapter.kernel.GetChangePlan(ctx, identity, runID)
	if err != nil {
		return nil, mapFactoryKernelError(err)
	}
	// Re-drive after a crash that left the run between PLANNING and RUNNING
	// (driveExecution durably commits PLANNING→READY then READY→RUNNING).
	if factoryShouldDriveExecution(plan.GetState()) {
		if err := adapter.driveExecution(ctx, identity, plan); err != nil {
			// driveExecution already collapses kernel denials and the stale
			// safe-point into ErrUnknownRun; unexpected pipeline faults stay
			// port failures so they never masquerade as a candidate fact.
			if errors.Is(err, factoryapi.ErrUnknownRun) {
				return nil, err
			}
			return nil, mapFactoryKernelError(err)
		}
	}
	preview, err := adapter.kernel.PreviewChangeSet(ctx, identity, runID)
	if err != nil {
		return nil, mapFactoryKernelError(err)
	}
	return preview, nil
}

// ReviewFindings drives the fresh review lazily on the first findings read
// once the candidate is verified: the deterministic reviewer records and
// dispositions the descriptor's typed findings, retains a clean candidate, or
// rejects a candidate carrying an undisposed blocker with its rollback
// receipt, then serves the canonical findings page.
func (adapter *factoryKernelAdapter) ReviewFindings(
	ctx context.Context, principal factoryapi.Principal, runID, after string, limit uint32,
) (factoryapi.FindingsPage, error) {
	if adapter.kernel == nil || ctx == nil {
		return factoryapi.FindingsPage{}, factoryapi.ErrUnknownRun
	}
	identity := factoryIdentity(principal)
	plan, err := adapter.kernel.GetChangePlan(ctx, identity, runID)
	if err != nil {
		return factoryapi.FindingsPage{}, mapFactoryKernelError(err)
	}
	// Re-drive after a crash that left the run between REVIEW and COMPLETED
	// (driveReview may have retained the candidate before finishing the run).
	if factoryShouldDriveReview(plan.GetState()) {
		if err := adapter.driveReview(ctx, identity, plan); err != nil {
			return factoryapi.FindingsPage{}, err
		}
	}
	page, err := adapter.kernel.GetReviewFindings(ctx, identity, runID, after, limit)
	if err != nil {
		return factoryapi.FindingsPage{}, mapFactoryKernelError(err)
	}
	return factoryapi.FindingsPage{Findings: page.Findings, NextCursor: page.NextCursor}, nil
}

// CancelChangeRun revokes one admitted run at a safe point; an exact
// idempotent replay returns the original terminal outcome.
func (adapter *factoryKernelAdapter) CancelChangeRun(
	ctx context.Context, command factoryapi.CancelRunCommand,
) (*contractsv1.CancelChangeRunSuccess, error) {
	if adapter.kernel == nil || ctx == nil {
		return nil, factoryapi.ErrUnknownRun
	}
	identity := factoryIdentity(command.Principal)
	cancelled, err := adapter.kernel.CancelChangeRun(ctx, brain.FactoryCancelRequest{
		Authenticated:  identity,
		Caller:         factoryCaller(identity),
		RunID:          command.RunID,
		IdempotencyKey: command.IdempotencyKey,
	})
	if err != nil {
		return nil, mapFactoryKernelError(err)
	}
	return &contractsv1.CancelChangeRunSuccess{
		RunId: &contractsv1.Identifier{Namespace: "factory-run", Value: cancelled.RunID},
		State: contractsv1.ChangeRunState_CHANGE_RUN_STATE_CANCELLED,
	}, nil
}

func factoryIdentity(principal factoryapi.Principal) brain.FactoryIdentity {
	return brain.FactoryIdentity{
		Tenant:    principal.Tenant,
		Principal: principal.PrincipalID,
		Session:   principal.Session,
	}
}

func factoryCaller(identity brain.FactoryIdentity) brain.FactoryCallerCrossCheck {
	return brain.FactoryCallerCrossCheck{
		Tenant:    identity.Tenant,
		Principal: identity.Principal,
		Session:   identity.Session,
	}
}

// errFactoryPortFailure collapses any unexpected kernel failure to a static
// port failure; the handler maps it to its static denial without upstream text.
var errFactoryPortFailure = errors.New("factory port failure")

// mapFactoryKernelError collapses every kernel denial to the one static
// non-disclosing port failure; anything else is a port failure.
func mapFactoryKernelError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, brain.ErrFactoryNotFoundOrDenied),
		errors.Is(err, brain.ErrFactoryStaleFence),
		errors.Is(err, brain.ErrFactoryResultConflict),
		errors.Is(err, brain.ErrFactoryScopeEscape),
		errors.Is(err, brain.ErrFactoryReviewerConflict):
		return factoryapi.ErrUnknownRun
	default:
		return fmt.Errorf("factory kernel: %w", errFactoryPortFailure)
	}
}

// factoryAuthorityAdapter mounts the factoryapi handler behind the gateway's
// authenticated transport, mirroring the Stage 04 query adapter exactly.
type factoryAuthorityAdapter struct {
	handler *factoryapi.Handler
}

func (adapter factoryAuthorityAdapter) AdmitChangeIntent(
	ctx context.Context, peer gateway.PeerContext, request *contractsv1.AdmitChangeIntentRequest,
) (*contractsv1.AdmitChangeIntentResponse, error) {
	return adapter.handler.AdmitChangeIntent(ctx, peer, request)
}

func (adapter factoryAuthorityAdapter) GetChangePlan(
	ctx context.Context, peer gateway.PeerContext, request *contractsv1.GetChangePlanRequest,
) (*contractsv1.GetChangePlanResponse, error) {
	return adapter.handler.GetChangePlan(ctx, peer, request)
}

func (adapter factoryAuthorityAdapter) PreviewChangeSet(
	ctx context.Context, peer gateway.PeerContext, request *contractsv1.PreviewChangeSetRequest,
) (*contractsv1.PreviewChangeSetResponse, error) {
	return adapter.handler.PreviewChangeSet(ctx, peer, request)
}

func (adapter factoryAuthorityAdapter) GetReviewFindings(
	ctx context.Context, peer gateway.PeerContext, request *contractsv1.GetReviewFindingsRequest,
) (*contractsv1.GetReviewFindingsResponse, error) {
	return adapter.handler.GetReviewFindings(ctx, peer, request)
}

func (adapter factoryAuthorityAdapter) CancelChangeRun(
	ctx context.Context, peer gateway.PeerContext, request *contractsv1.CancelChangeRunRequest,
) (*contractsv1.CancelChangeRunResponse, error) {
	return adapter.handler.CancelChangeRun(ctx, peer, request)
}

var _ gateway.FactoryAuthority = factoryAuthorityAdapter{}

// factoryLeaseTTLFromEnv resolves the leaf lease TTL: the bounded deterministic
// default applies unless the operator explicitly pins a TTL inside the bounded
// range. A malformed or out-of-range value rejects startup, fail-closed.
func factoryLeaseTTLFromEnv(getenv func(string) string) (int64, error) {
	if getenv == nil {
		return 0, errInvalidConfig
	}
	raw := strings.TrimSpace(getenv("OUROBOROS_FACTORY_LEASE_TTL_MS"))
	if raw == "" {
		return factoryDefaultLeaseTTLMillis, nil
	}
	parsed, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || parsed < factoryMinLeaseTTLMillis || parsed > factoryMaxLeaseTTLMillis {
		return 0, errInvalidConfig
	}
	return parsed, nil
}
