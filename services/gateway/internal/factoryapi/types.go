package factoryapi

import (
	"errors"

	contractsv1 "github.com/sltbrta/sentra-code-memory-v2/packages/contracts/gen/go/ouroboros/contracts/v1"
)

var (
	// ErrRequestDenied marks an authenticated-context failure: an unmapped or
	// malformed peer identity, or a body identity that does not match the
	// authenticated peer. The transport maps it to the static request-denied
	// shape; no port is ever invoked.
	ErrRequestDenied = errors.New("factoryapi: request denied")
	// ErrInvalidRequest marks a message that fails decoding or Protovalidate
	// before authority work. The transport maps it to the static
	// request-malformed shape.
	ErrInvalidRequest = errors.New("factoryapi: invalid request")
	// ErrInvalidResponse marks a constructed response that fails contract
	// validation — a defect in a composed port or in this package. The
	// transport maps it to the static response-invalid shape.
	ErrInvalidResponse = errors.New("factoryapi: invalid response")
	// ErrUnknownRun is the non-disclosing port failure for an unknown,
	// unauthorized, stale, or revoked run, and for a stale base, stale lease
	// or fence, or revoked grant on admission. Ports must return it without
	// existence detail; the handler maps it to the static not_found_or_denied
	// outcome.
	ErrUnknownRun = errors.New("factoryapi: unknown run")
	// ErrIdempotencyConflict marks a reused idempotency key bound to a
	// different request digest. The handler maps it to the static
	// not_found_or_denied outcome without mutation.
	ErrIdempotencyConflict = errors.New("factoryapi: idempotency conflict")
	// ErrInvalidConfiguration marks an incomplete or malformed handler
	// configuration at construction time.
	ErrInvalidConfiguration = errors.New("factoryapi: invalid configuration")
	// errPortFailure marks any other port failure the response contract has
	// no shape for; the transport maps it to its static denial. It never
	// wraps port error text.
	errPortFailure = errors.New("factoryapi: port failure")
)

// Principal is the authenticated gateway peer identity, derived exclusively
// from the peer the transport authenticated — never from request bodies.
type Principal struct {
	Tenant      string
	PrincipalID string
	Session     string
}

// AdmitIntentCommand is one admission of an approved change intent under the
// authenticated principal scope. Intent passes the frozen buf.validate and
// CEL rules before the port ever sees it; the port revalidates approval, base,
// and evidence under current policy before opening the run.
type AdmitIntentCommand struct {
	Principal      Principal
	Intent         *contractsv1.ChangeIntent
	IdempotencyKey string
}

// CancelRunCommand is one idempotent revocation of one admitted run under the
// authenticated principal scope.
type CancelRunCommand struct {
	Principal      Principal
	RunID          string
	IdempotencyKey string
}

// FindingsPage is one bounded authorized review-finding page with its opaque
// cursor. Findings carry only the frozen ReviewFinding vocabulary owned by
// the authorized run; an empty page with no cursor is a valid terminal page.
type FindingsPage struct {
	Findings   []*contractsv1.ReviewFinding
	NextCursor string
}
