package meeting

import "errors"

var (
	// ErrInvalidInput reports missing or malformed kernel facts presented by
	// the caller. Unlike denials it is a programming error at the boundary and
	// is safe to surface to operators, never to untrusted clients.
	ErrInvalidInput = errors.New("meeting: invalid input")
	// ErrNotFoundOrDenied is the single static non-disclosing denial: absent,
	// unauthorized, revoked, purged, and conflicting operations share it.
	ErrNotFoundOrDenied = errors.New("meeting: not found or denied")
	// ErrSchemaUnsupported reports an authority database missing migration 006.
	ErrSchemaUnsupported = errors.New("meeting: schema unsupported")
	// ErrPayloadUnavailable reports an unreadable or digest-mismatched vault
	// payload; reads fail closed rather than serve unverified bytes.
	ErrPayloadUnavailable = errors.New("meeting: payload unavailable")
)
