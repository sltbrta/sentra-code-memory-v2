package multimodal

import "errors"

var (
	// ErrInvalidInput reports missing or malformed kernel facts presented by
	// the caller. Unlike denials it is a programming error at the boundary and
	// is safe to surface to operators, never to untrusted clients.
	ErrInvalidInput = errors.New("multimodal: invalid input")
	// ErrNotFoundOrDenied is the single static non-disclosing denial: absent,
	// unauthorized, revoked, purged, and conflicting operations share it.
	ErrNotFoundOrDenied = errors.New("multimodal: not found or denied")
	// ErrSchemaUnsupported reports an authority database missing migration 007.
	ErrSchemaUnsupported = errors.New("multimodal: schema unsupported")
	// ErrPayloadUnavailable reports an unreadable or digest-mismatched vault
	// payload; reads fail closed rather than serve unverified bytes.
	ErrPayloadUnavailable = errors.New("multimodal: payload unavailable")
	// ErrOversized is a fail-loud pre-decode denial for size/page/MP/duration bounds.
	ErrOversized = errors.New("multimodal: oversized")
	// ErrMalformed is a fail-loud pre-decode denial for corrupt containers.
	ErrMalformed = errors.New("multimodal: malformed")
	// ErrMediaTypeMismatch is a fail-loud pre-decode denial for kind/media mismatch.
	ErrMediaTypeMismatch = errors.New("multimodal: media type mismatch")
	// ErrEncryptedOrUnsupported is a fail-loud pre-decode denial for codecs
	// outside the Stage 11 v1 set (JPEG, compressed audio, video, …).
	ErrEncryptedOrUnsupported = errors.New("multimodal: encrypted or unsupported")
	// ErrPartialPayload is a fail-loud pre-decode denial when declared length
	// exceeds available bytes or the payload is truncated.
	ErrPartialPayload = errors.New("multimodal: partial payload")
)
