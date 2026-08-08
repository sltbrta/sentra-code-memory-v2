package authz

import (
	"context"

	"github.com/sltbrta/sentra-code-memory-v2/services/internal/contracts"
)

// RelationshipStore is the policy surface used by the local-authority Broker.
// Implementations must fail closed on missing, stale, malformed, or unavailable
// facts. Tenant membership alone never authorizes evidence or brain actions.
type RelationshipStore interface {
	Write(tuple Tuple) error
	Delete(tuple Tuple) (tenant string, epoch uint64, err error)
	SetEpoch(tenant string, epoch uint64) error
	Epoch(tenant string) (uint64, error)
	Check(ctx context.Context, identity contracts.MappedIdentityFact, request contracts.PolicyRequest) (contracts.PolicyDecision, error)
	CheckSource(ctx context.Context, identity contracts.MappedIdentityFact, action string, brain contracts.Identifier) (contracts.PolicyDecision, error)
}

// InProcessAdapter wraps Evaluator as the default RelationshipStore.
// This is the fail-closed path localauthority.Broker uses when no remote
// OpenFGA URL is configured.
type InProcessAdapter struct {
	eval *Evaluator
}

// NewInProcessAdapter returns an empty default-deny in-process store.
func NewInProcessAdapter() *InProcessAdapter {
	return &InProcessAdapter{eval: NewEvaluator()}
}

// NewInProcessAdapterFrom wraps an existing evaluator. A nil evaluator becomes
// a fresh default-deny instance.
func NewInProcessAdapterFrom(eval *Evaluator) *InProcessAdapter {
	if eval == nil {
		eval = NewEvaluator()
	}
	return &InProcessAdapter{eval: eval}
}

// Evaluator exposes the underlying evaluator for tests that need the concrete type.
func (a *InProcessAdapter) Evaluator() *Evaluator {
	if a == nil {
		return nil
	}
	return a.eval
}

// Write implements RelationshipStore.
func (a *InProcessAdapter) Write(tuple Tuple) error {
	if a == nil || a.eval == nil {
		return ErrMalformedTuple
	}
	return a.eval.Write(tuple)
}

// Delete implements RelationshipStore.
func (a *InProcessAdapter) Delete(tuple Tuple) (string, uint64, error) {
	if a == nil || a.eval == nil {
		return "", 0, ErrMalformedTuple
	}
	return a.eval.Delete(tuple)
}

// SetEpoch implements RelationshipStore.
func (a *InProcessAdapter) SetEpoch(tenant string, epoch uint64) error {
	if a == nil || a.eval == nil {
		return ErrMalformedTuple
	}
	return a.eval.SetEpoch(tenant, epoch)
}

// Epoch implements RelationshipStore.
func (a *InProcessAdapter) Epoch(tenant string) (uint64, error) {
	if a == nil || a.eval == nil {
		return 0, ErrMalformedTuple
	}
	return a.eval.Epoch(tenant)
}

// Check implements RelationshipStore.
func (a *InProcessAdapter) Check(ctx context.Context, identity contracts.MappedIdentityFact, request contracts.PolicyRequest) (contracts.PolicyDecision, error) {
	if a == nil || a.eval == nil {
		return contracts.PolicyDecision{Receipt: deniedReceipt(), Allowed: false}, nil
	}
	return a.eval.Check(ctx, identity, request)
}

// CheckSource implements RelationshipStore.
func (a *InProcessAdapter) CheckSource(ctx context.Context, identity contracts.MappedIdentityFact, action string, brain contracts.Identifier) (contracts.PolicyDecision, error) {
	if a == nil || a.eval == nil {
		return contracts.PolicyDecision{Receipt: deniedReceipt(), Allowed: false}, nil
	}
	return a.eval.CheckSource(ctx, identity, action, brain)
}

var _ RelationshipStore = (*InProcessAdapter)(nil)
