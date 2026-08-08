package tracer001

import (
	"context"
	"time"
)

// Path is the composition port the Stage 06 integration leaf satisfies. Each
// method advances one public Tracer 001 step under the authenticated principal.
// Unknown, unauthorized, stale, or revoked scopes return ErrUnknownScope
// without existence detail; conflicting idempotency reuse returns
// ErrIdempotencyConflict; cancellation returns the context error unwrapped.
// Exact authenticated idempotent replays return the original outcome.
type Path interface {
	Advance(ctx context.Context, command StepCommand) (*PathSuccess, error)
}

// Clock supplies receipt time without ambient time.Now.
type Clock interface {
	Now() time.Time
}
