package connectorapi

import (
	"errors"

	contractsv1 "github.com/sltbrta/sentra-code-memory-v2/packages/contracts/gen/go/ouroboros/contracts/v1"
)

var (
	// ErrRequestDenied marks an authenticated-context failure.
	ErrRequestDenied = errors.New("connectorapi: request denied")
	// ErrInvalidRequest marks a message that fails decoding or Protovalidate.
	ErrInvalidRequest = errors.New("connectorapi: invalid request")
	// ErrInvalidResponse marks a constructed response that fails contract validation.
	ErrInvalidResponse = errors.New("connectorapi: invalid response")
	// ErrUnknownConnection is the non-disclosing port failure for unknown,
	// unauthorized, revoked, or purged connections.
	ErrUnknownConnection = errors.New("connectorapi: unknown connection")
	// ErrIdempotencyConflict marks a reused key bound to a different request digest.
	ErrIdempotencyConflict = errors.New("connectorapi: idempotency conflict")
	// ErrInvalidConfiguration marks incomplete handler configuration.
	ErrInvalidConfiguration = errors.New("connectorapi: invalid configuration")
	errPortFailure          = errors.New("connectorapi: port failure")
)

// Principal is the authenticated gateway peer identity.
type Principal struct {
	Tenant      string
	PrincipalID string
	Session     string
}

// ConnectCommand is one GitHub source connect under the authenticated principal.
type ConnectCommand struct {
	Principal Principal
	Request   *contractsv1.ConnectGitHubSourceRequest
}

// StatusCommand is one status read under the authenticated principal.
type StatusCommand struct {
	Principal    Principal
	ConnectionID string
}

// ReconcileCommand is one reconcile under the authenticated principal.
type ReconcileCommand struct {
	Principal Principal
	Request   *contractsv1.ReconcileConnectorRequest
}

// QueryCommand is one evidence query under the authenticated principal.
type QueryCommand struct {
	Principal Principal
	Request   *contractsv1.QueryConnectorEvidenceRequest
}

// RevokeCommand is one revoke under the authenticated principal.
type RevokeCommand struct {
	Principal      Principal
	ConnectionID   string
	IdempotencyKey string
}

// PurgeCommand is one purge under the authenticated principal.
type PurgeCommand struct {
	Principal      Principal
	ConnectionID   string
	IdempotencyKey string
}
