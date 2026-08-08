package queryapi

import (
	"context"
	"time"
)

// Engine is the bounded grounded-query port the L1 engine satisfies. Answer
// composes every in-contract outcome — answered, partial, abstained — as an
// EngineResult; unknown, revoked, or unservable scopes return ErrUnknownScope
// and caller cancellation returns the context error unwrapped. Status shares
// the same scope rule because a status read has no abstention shape.
type Engine interface {
	Answer(ctx context.Context, query EngineQuery) (EngineResult, error)
	Status(ctx context.Context, principal Principal, sourceID string) (EngineStatus, error)
}

// Conversations is the durable private conversation port the migration 004
// store satisfies. Admission commits user turn plus idempotency record
// atomically; completion is exactly once per admitted key; resolution returns
// the original outcome for an idempotent replay; history returns only the
// authenticated principal's own turns in the frozen total order.
type Conversations interface {
	Admit(ctx context.Context, admission Admission) (AdmissionResult, error)
	Complete(ctx context.Context, completion Completion) (CompletionResult, error)
	Resolve(ctx context.Context, tenant, principal, idempotencyKey string) (Resolution, error)
	History(ctx context.Context, tenant, principal, after string, limit uint32) (HistoryPage, error)
}

// SourceCatalog serves authorized, non-disclosing source and generation
// metadata. Implementations must return ErrUnknownScope for unknown,
// unauthorized, or revoked scopes and must never include revoked sources in
// listings. Generation facts are immutable per generation identity, so a
// superseded pinned generation resolves exactly as a current one.
type SourceCatalog interface {
	List(ctx context.Context, principal Principal, after string, limit uint32) (SourcePage, error)
	Facts(ctx context.Context, principal Principal, sourceID, generationID string) (GenerationFacts, error)
	Reference(ctx context.Context, principal Principal, sourceID string) (SourceFacts, error)
}

// Authorizer evaluates the principal's current relationships immediately
// before each guarded effect. Any error is treated as denial, and denial
// never widens scope. GetHistory reauthorizes with ActionHydrate on the
// history scope before any payload hydration.
type Authorizer interface {
	Authorize(ctx context.Context, principal Principal, action Action, resource string) (Decision, error)
}

// Clock supplies receipt and observation time without ambient time.Now.
type Clock interface {
	Now() time.Time
}

// historyScope is the authorization resource naming the authenticated
// principal's own private conversation lane. Cross-principal history is
// structurally inexpressible; this checkpoint reauthorizes hydration against
// current policy exactly as the frozen metadata section requires.
const historyScope = "conversation"
