package factoryapi

import (
	"context"
	"time"

	contractsv1 "github.com/sltbrta/sentra-code-memory-v2/packages/contracts/gen/go/ouroboros/contracts/v1"
)

// Kernel is the bounded Stage 05 factory-authority port the deterministic
// kernel (leaf #135) satisfies. The port speaks the frozen contract
// vocabulary because the kernel's canonical run, plan, candidate, and finding
// state commits under exactly these identities; the gateway never translates,
// stores, or reinterprets factory domain state, it only authenticates,
// validates, and binds receipts.
//
// Authorization and idempotency are kernel obligations inside the port:
// AdmitChangeIntent revalidates the intent's approval, exact Git base, and
// supporting evidence under current policy before opening a run, and an exact
// authenticated idempotent replay returns the original outcome without
// re-executing. Unknown, unauthorized, stale, or revoked runs — including a
// stale base, a stale lease or fence, and a revoked grant — return
// ErrUnknownRun without existence detail; a conflicting idempotency-key reuse
// returns ErrIdempotencyConflict; caller cancellation returns the context
// error unwrapped. Any other error is treated as a port failure and never
// crosses the public boundary with its text.
type Kernel interface {
	// AdmitChangeIntent opens one run for one approved intent, or returns the
	// original outcome for an exact idempotent replay. The returned success
	// opens only in PLANNING or READY state as the frozen contract requires.
	AdmitChangeIntent(ctx context.Context, command AdmitIntentCommand) (*contractsv1.AdmitChangeIntentSuccess, error)
	// ChangePlan reads the current typed one-layer DAG and gate roster for
	// one admitted run under the authenticated scope.
	ChangePlan(ctx context.Context, principal Principal, runID string) (*contractsv1.ChangePlan, error)
	// ChangeSetPreview reads the atomic exact-base candidate preview with
	// per-language obligations, gate roster, and rollback facts.
	ChangeSetPreview(ctx context.Context, principal Principal, runID string) (*contractsv1.ChangeSetPreview, error)
	// ReviewFindings pages typed fresh-review findings for one admitted run
	// in the frozen total order; page size is contract-bounded before lookup.
	ReviewFindings(ctx context.Context, principal Principal, runID, after string, limit uint32) (FindingsPage, error)
	// CancelChangeRun revokes one admitted run at a safe point and denies
	// pending effects, or returns the original outcome for an exact replay.
	// The returned success confirms only the terminal CANCELLED state.
	CancelChangeRun(ctx context.Context, command CancelRunCommand) (*contractsv1.CancelChangeRunSuccess, error)
}

// Clock supplies receipt and observation time without ambient time.Now.
type Clock interface {
	Now() time.Time
}
