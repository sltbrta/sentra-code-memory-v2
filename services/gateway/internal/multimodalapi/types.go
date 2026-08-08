package multimodalapi

import (
	"errors"

	contractsv1 "github.com/sltbrta/sentra-code-memory-v2/packages/contracts/gen/go/ouroboros/contracts/v1"
)

var (
	// ErrRequestDenied marks an authenticated-context failure.
	ErrRequestDenied = errors.New("multimodalapi: request denied")
	// ErrInvalidRequest marks a message that fails decoding or Protovalidate.
	ErrInvalidRequest = errors.New("multimodalapi: invalid request")
	// ErrInvalidResponse marks a constructed response that fails contract validation.
	ErrInvalidResponse = errors.New("multimodalapi: invalid response")
	// ErrUnknownSource is the non-disclosing port failure for unknown,
	// unauthorized, revoked, or purged sources.
	ErrUnknownSource = errors.New("multimodalapi: unknown source")
	// ErrIdempotencyConflict marks a reused key bound to a different request digest.
	ErrIdempotencyConflict = errors.New("multimodalapi: idempotency conflict")
	// ErrInvalidConfiguration marks incomplete handler configuration.
	ErrInvalidConfiguration = errors.New("multimodalapi: invalid configuration")
	// ErrOversized is a fail-loud pre-decode denial.
	ErrOversized = errors.New("multimodalapi: oversized")
	// ErrMalformed is a fail-loud pre-decode denial.
	ErrMalformed = errors.New("multimodalapi: malformed")
	// ErrMediaTypeMismatch is a fail-loud pre-decode denial.
	ErrMediaTypeMismatch = errors.New("multimodalapi: media type mismatch")
	// ErrEncryptedOrUnsupported is a fail-loud pre-decode denial.
	ErrEncryptedOrUnsupported = errors.New("multimodalapi: encrypted or unsupported")
	// ErrPartialPayload is a fail-loud pre-decode denial.
	ErrPartialPayload = errors.New("multimodalapi: partial payload")
	errPortFailure    = errors.New("multimodalapi: port failure")
)

// Principal is the authenticated gateway peer identity.
type Principal struct {
	Tenant      string
	PrincipalID string
	Session     string
}

// AdmitCommand is one multimodal admit under the authenticated principal.
type AdmitCommand struct {
	Principal    Principal
	Request      *contractsv1.AdmitMultimodalSourceRequest
	ForcePartial bool
}

// StatusCommand is one status read under the authenticated principal.
type StatusCommand struct {
	Principal Principal
	SourceID  string
}

// EvidenceCommand is one evidence page under the authenticated principal.
type EvidenceCommand struct {
	Principal Principal
	SourceID  string
	PageSize  uint32
	After     string
}

// RevokeCommand is one revoke under the authenticated principal.
type RevokeCommand struct {
	Principal      Principal
	SourceID       string
	IdempotencyKey string
}

// PurgeCommand is one purge under the authenticated principal.
type PurgeCommand struct {
	Principal      Principal
	SourceID       string
	IdempotencyKey string
}
