package factory

import "errors"

var (
	// ErrInvalidInput reports missing or malformed kernel facts presented by
	// the caller. Unlike denials it is a programming error at the boundary and
	// is safe to surface to operators, never to untrusted clients.
	ErrInvalidInput = errors.New("factory: invalid input")
	// ErrNotFoundOrDenied is the single static non-disclosing denial: absent,
	// unauthorized, stale, revoked, and conflicting operations share it, so no
	// existence detail crosses the kernel boundary.
	ErrNotFoundOrDenied = errors.New("factory: not found or denied")
	// ErrSchemaUnsupported reports an authority database missing migration 005.
	ErrSchemaUnsupported = errors.New("factory: schema unsupported")
	// ErrPayloadUnavailable reports an unreadable or digest-mismatched vault
	// payload; reads fail closed rather than serve unverified bytes.
	ErrPayloadUnavailable = errors.New("factory: payload unavailable")
	// ErrPlanInvalid reports a proposed DAG violating the frozen one-layer
	// shape: overlapping leaf scopes, out-of-scope writes, dispatch authority,
	// or structural CEL violations.
	ErrPlanInvalid = errors.New("factory: plan invalid")
	// ErrTransitionInvalid reports a lifecycle transition outside the bounded
	// run, gate, or candidate progressions.
	ErrTransitionInvalid = errors.New("factory: transition invalid")
	// ErrReviewerConflict reports a review identity that collides with a leaf
	// grant initiator, which the frozen contract forbids.
	ErrReviewerConflict = errors.New("factory: reviewer conflict")
	// ErrScopeEscape reports a candidate edit outside every leaf's owned write
	// scope; the proposal denies before any canonical fact commits.
	ErrScopeEscape = errors.New("factory: scope escape")
)
