package connector

import (
	"time"

	contractsv1 "github.com/sltbrta/sentra-code-memory-v2/packages/contracts/gen/go/ouroboros/contracts/v1"
)

// Identity is the authenticated principal scope from the gateway peer.
type Identity struct {
	Tenant    string
	Principal string
	Session   string
}

// ConnectCommand admits one GitHub source scope under the authenticated principal.
type ConnectCommand struct {
	Identity Identity
	Request  *contractsv1.ConnectGitHubSourceRequest
}

// StatusCommand reads one connection under the authenticated principal.
type StatusCommand struct {
	Identity     Identity
	ConnectionID string
}

// ReconcileCommand advances one connection from a known cursor.
type ReconcileCommand struct {
	Identity Identity
	Request  *contractsv1.ReconcileConnectorRequest
}

// QueryCommand answers one question against admitted evidence.
type QueryCommand struct {
	Identity Identity
	Request  *contractsv1.QueryConnectorEvidenceRequest
	// DelegatedGrantID names the explicit delegated-permission grant used for
	// ACL-opaque source scopes (issue #309). Empty is valid for non-opaque
	// scopes; for opaque scopes it fails closed with a delegated abstain.
	DelegatedGrantID string
}

// RevokeCommand denies one connection under the authenticated principal.
type RevokeCommand struct {
	Identity       Identity
	ConnectionID   string
	IdempotencyKey string
}

// PurgeCommand purges one connection under the authenticated principal.
type PurgeCommand struct {
	Identity       Identity
	ConnectionID   string
	IdempotencyKey string
}

// Config binds a Kernel to its provider surface.
type Config struct {
	// Source is the GitHub source API (FakeSourceAPI or gateway-injected live).
	Source SourceAPI
	// Delegated optionally gates ACL-opaque source scopes behind explicit
	// delegated-permission grants (issue #309). Nil leaves every scope on the
	// existing projection-membership behavior.
	Delegated *DelegatedGate
}

type connectionState string

const (
	stateReady    connectionState = "READY"
	stateDegraded connectionState = "DEGRADED"
	stateRevoked  connectionState = "REVOKED"
	statePurged   connectionState = "PURGED"
)

type connection struct {
	id              string
	tenant          string
	principal       string
	session         string
	owner           string
	repo            string
	sourceScope     string
	state           connectionState
	cursor          string
	sourceRevision  string
	connectorDigest string
	observedAt      time.Time
	aclEpoch        uint64
	lastError       string
	objects         map[string]Object
	requestDigest   string
	connectKey      string
}

type idempotencyRow struct {
	operation     string
	requestDigest string
	connectionID  string
}
