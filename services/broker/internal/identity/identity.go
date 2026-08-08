// Package identity maps trusted operating-system peer facts to local sessions.
// Request bodies may repeat identity only for an exact, fail-closed cross-check.
package identity

import (
	"errors"

	"github.com/sltbrta/sentra-code-memory-v2/services/internal/contracts"
)

var (
	// ErrDenied is the non-disclosing result for missing or mismatched identity.
	ErrDenied = errors.New("identity: denied")
)

// Mapper binds one local operating-system user to an authority identity.
type Mapper struct {
	uid       uint32
	principal contracts.Identifier
	tenant    contracts.Identifier
}

// NewMapper constructs a mapper after validating required identity namespaces.
func NewMapper(uid uint32, principal, tenant contracts.Identifier) (*Mapper, error) {
	if !validID(principal, "principal") || !validID(tenant, "tenant") {
		return nil, ErrDenied
	}
	return &Mapper{uid: uid, principal: principal, tenant: tenant}, nil
}

// Map authenticates peer credentials before any request-body identity is used.
// PID must be non-zero and UID must exactly match the configured local owner.
func (m *Mapper) Map(credentials contracts.PeerCredentials, session contracts.Identifier) (contracts.MappedIdentityFact, error) {
	if m == nil || credentials.UID != m.uid || credentials.PID == 0 || !validID(session, "session") {
		return contracts.MappedIdentityFact{}, ErrDenied
	}
	return contracts.MappedIdentityFact{
		Principal:   m.principal,
		Tenant:      m.tenant,
		Session:     session,
		Credentials: credentials,
	}, nil
}

// CrossCheckCommand rejects a command actor unless principal, tenant, and session
// exactly match the authenticated peer session.
func CrossCheckCommand(authenticated contracts.MappedIdentityFact, bodyPrincipal, bodyTenant, bodySession contracts.Identifier) error {
	if !validID(authenticated.Principal, "principal") || !validID(authenticated.Tenant, "tenant") ||
		!validID(authenticated.Session, "session") || bodyPrincipal != authenticated.Principal ||
		bodyTenant != authenticated.Tenant || bodySession != authenticated.Session {
		return ErrDenied
	}
	return nil
}

// CrossCheckStatus rejects a status request for any session other than the authenticated one.
func CrossCheckStatus(authenticated contracts.MappedIdentityFact, requestedSession contracts.Identifier) error {
	if !validID(authenticated.Session, "session") || requestedSession != authenticated.Session {
		return ErrDenied
	}
	return nil
}

func validID(identifier contracts.Identifier, namespace string) bool {
	return identifier.Namespace == namespace && identifier.Value != ""
}
