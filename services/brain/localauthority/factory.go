package localauthority

import (
	"context"

	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/factory"
	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/factory/roster"
	shared "github.com/sltbrta/sentra-code-memory-v2/services/internal/contracts"
)

// Public aliases over the bounded Stage 05 factory kernel shapes. They let the
// composing gateway command wire the production factory surface without
// importing brain-internal packages; every alias is the exact internal type,
// so invariants are never re-declared.
type (
	FactoryKernel           = factory.Kernel
	FactoryIdentity         = factory.Identity
	FactoryCallerCrossCheck = factory.CallerCrossCheck
	FactoryAdmitRequest     = factory.AdmitRequest
	FactoryAdmitResult      = factory.AdmitResult
	FactoryLeafSpec         = factory.LeafSpec
	FactoryCancelRequest    = factory.CancelRequest
	FactoryCancelResult     = factory.CancelResult
	FactoryFindingsPage     = factory.FindingsPage
	FactoryFindingDraft     = factory.FindingDraft
	FactoryFindingResult    = factory.FindingResult
	FactoryRollbackReceipt  = factory.RollbackReceipt
)

var (
	// ErrFactoryNotFoundOrDenied is the single static non-disclosing kernel
	// denial: absent, unauthorized, stale, or revoked runs are indistinguishable.
	ErrFactoryNotFoundOrDenied = factory.ErrNotFoundOrDenied
	// ErrFactoryInvalidInput marks malformed kernel boundary facts.
	ErrFactoryInvalidInput = factory.ErrInvalidInput
	// ErrFactoryPlanInvalid marks a proposed DAG violating the frozen shape.
	ErrFactoryPlanInvalid = factory.ErrPlanInvalid
	// ErrFactoryTransitionInvalid marks a lifecycle transition outside the
	// bounded run or candidate shape.
	ErrFactoryTransitionInvalid = factory.ErrTransitionInvalid
	// ErrFactoryScopeEscape marks a candidate edit outside every leaf scope.
	ErrFactoryScopeEscape = factory.ErrScopeEscape
	// ErrFactoryReviewerConflict marks a review identity colliding with a leaf
	// grant initiator.
	ErrFactoryReviewerConflict = factory.ErrReviewerConflict
	// ErrFactoryStaleFence marks a commit under an expired or superseded fence.
	ErrFactoryStaleFence = roster.ErrStaleFence
	// ErrFactoryResultConflict marks a second differing leaf result commit.
	ErrFactoryResultConflict = roster.ErrResultConflict
)

// FactorySurfaceConfig binds the composed Stage 05 factory surface. Policy is
// the current-policy check port the composing command binds to the production
// broker; LeaseTTLMillis bounds issued leaf leases; RevocationEpoch and
// PolicyDigestHex pin the deny-overlay epoch and policy digest observed at
// composition onto every issued leaf grant.
type FactorySurfaceConfig struct {
	Policy          shared.PolicyCheck
	LeaseTTLMillis  int64
	RevocationEpoch uint64
	PolicyDigestHex string
}

// FactorySurface is the composed Stage 05 factory surface: the deterministic
// kernel over the durable authority, the Stage 02 encrypted payload vault, the
// Stage 03 ingestion catalog for exact base resolution, and the current-policy
// port. Close releases only the kernel handle; the owning Runtime closes the
// authority itself.
type FactorySurface struct {
	runtime *Runtime
	kernel  *factory.Kernel
}

// OpenFactorySurface composes the Stage 05 factory kernel over one durable
// ingestion-configured runtime. The kernel reuses the same migrated authority
// database (migration 005 verified by the kernel), the same encrypted payload
// vault as the Stage 04 conversation store, and the same authority clock; the
// exact Git base resolves from the Stage 03 ingestion catalog on every
// admission, and leaf routing pins the deterministic certified profile. The
// call fails closed without a durable payload vault, a database path, or the
// ingestion runtime: the factory surface widens the served surface of the same
// authority, never its trust boundary.
func (r *Runtime) OpenFactorySurface(ctx context.Context, config FactorySurfaceConfig) (*FactorySurface, error) {
	if r == nil || ctx == nil || config.Policy == nil || config.LeaseTTLMillis <= 0 ||
		len(config.PolicyDigestHex) != 64 {
		return nil, ErrInvalid
	}
	r.ingestionMu.RLock()
	configured := r.ingestion != nil
	r.ingestionMu.RUnlock()
	if !configured || r.databasePath == "" || r.conversationPayloads == nil || r.clock == nil {
		return nil, ErrInvalid
	}
	router, err := factory.NewStaticRouter(r.config.Hex, "deterministic-v1", "deterministic_v1")
	if err != nil {
		return nil, ErrInvalid
	}
	kernel, err := factory.Open(ctx, factory.Config{
		DatabasePath:    r.databasePath,
		Payloads:        r.conversationPayloads,
		Clock:           r.clock,
		Bases:           factoryBaseResolver{runtime: r},
		Policy:          config.Policy,
		Router:          router,
		LeaseTTLMillis:  config.LeaseTTLMillis,
		RevocationEpoch: config.RevocationEpoch,
		PolicyDigestHex: config.PolicyDigestHex,
	})
	if err != nil {
		return nil, ErrUnavailable
	}
	return &FactorySurface{runtime: r, kernel: kernel}, nil
}

// Kernel returns the composed deterministic factory kernel.
func (s *FactorySurface) Kernel() *factory.Kernel {
	if s == nil {
		return nil
	}
	return s.kernel
}

// Close releases the kernel database handle. It is idempotent and does not
// close the owning runtime.
func (s *FactorySurface) Close() error {
	if s == nil || s.kernel == nil {
		return nil
	}
	return s.kernel.Close()
}

// factoryBaseResolver is the Stage 03 exact-base adapter: the approved
// repository's current committed base resolves from the durable ingestion
// catalog — source state to the current published generation to its pinned
// commit — never from request facts. Any catalog failure fails closed, so a
// missing, unpublished, or revoked source makes admission deny statically.
type factoryBaseResolver struct {
	runtime *Runtime
}

// CurrentBase returns the exact current base commit of the configured
// approved source under the authenticated scope.
func (resolver factoryBaseResolver) CurrentBase(ctx context.Context, _ factory.Identity) (string, error) {
	if ctx == nil || resolver.runtime == nil {
		return "", ErrDenied
	}
	scope, err := resolver.runtime.ingestionScope()
	if err != nil {
		return "", err
	}
	state, err := resolver.runtime.store.LoadIngestionSourceState(ctx, scope)
	if err != nil || state.CurrentGenerationID == "" {
		return "", ErrDenied
	}
	facts, err := resolver.runtime.store.LoadIngestionGenerationFacts(ctx, scope, state.CurrentGenerationID)
	if err != nil {
		return "", ErrDenied
	}
	return facts.CommitOID, nil
}
