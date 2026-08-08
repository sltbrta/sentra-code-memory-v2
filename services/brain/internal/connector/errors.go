package connector

import "errors"

var (
	// ErrInvalidInput reports missing or malformed kernel facts.
	ErrInvalidInput = errors.New("connector: invalid input")
	// ErrNotFoundOrDenied is the single static non-disclosing denial.
	ErrNotFoundOrDenied = errors.New("connector: not found or denied")
	// ErrIdempotencyConflict marks a reused key bound to a different request.
	ErrIdempotencyConflict = errors.New("connector: idempotency conflict")
)
