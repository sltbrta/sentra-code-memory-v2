package factoryapi

import (
	"buf.build/go/protovalidate"
	contractsv1 "github.com/sltbrta/sentra-code-memory-v2/packages/contracts/gen/go/ouroboros/contracts/v1"
	"github.com/sltbrta/sentra-code-memory-v2/services/gateway/internal/localauthority"
	shared "github.com/sltbrta/sentra-code-memory-v2/services/internal/contracts"
	"google.golang.org/protobuf/proto"
)

// crossCheckPeer derives the trusted principal exclusively from the
// authenticated peer and rejects any body identity that disagrees with it.
// The check runs before any port invocation: an unmapped peer, a missing
// caller, or a principal, tenant, or session mismatch is ErrRequestDenied.
func crossCheckPeer(peer localauthority.PeerContext, caller *contractsv1.UntrustedFactoryCaller) (Principal, error) {
	identity := peer.Identity
	if !validMappedIdentity(identity) || caller == nil {
		return Principal{}, ErrRequestDenied
	}
	requested := caller.RequestedPrincipal
	if requested == nil || !sameIdentifier(requested.PrincipalId, identity.Principal) ||
		!sameIdentifier(requested.TenantId, identity.Tenant) ||
		!sameIdentifier(requested.SessionId, identity.Session) ||
		!sameIdentifier(caller.RequestedSession, identity.Session) {
		return Principal{}, ErrRequestDenied
	}
	return Principal{
		Tenant:      identity.Tenant.Value,
		PrincipalID: identity.Principal.Value,
		Session:     identity.Session.Value,
	}, nil
}

func validMappedIdentity(identity shared.MappedIdentityFact) bool {
	return identity.Principal.Namespace != "" && identity.Principal.Value != "" &&
		identity.Tenant.Namespace != "" && identity.Tenant.Value != "" &&
		identity.Session.Namespace != "" && identity.Session.Value != ""
}

func sameIdentifier(identifier *contractsv1.Identifier, expected shared.Identifier) bool {
	return identifier != nil && identifier.Namespace == expected.Namespace && identifier.Value == expected.Value
}

// sameContractIdentifier compares two contract identifiers on namespace and
// value; a nil side never matches.
func sameContractIdentifier(left, right *contractsv1.Identifier) bool {
	return left != nil && right != nil &&
		left.Namespace == right.Namespace && left.Value == right.Value
}

// validateRequest executes the generated buf.validate field, required-oneof,
// and CEL rules before any authority use, exactly as the frozen contract
// requires. Validation failures never reach a port.
func validateRequest(message proto.Message) error {
	if message == nil || !message.ProtoReflect().IsValid() {
		return ErrInvalidRequest
	}
	if err := protovalidate.Validate(message); err != nil {
		return ErrInvalidRequest
	}
	return nil
}

// validateResponse revalidates every constructed response against the frozen
// descriptors before return: run facts can never coexist with a rejection,
// and a defect fails closed instead of serving invalid contracts.
func validateResponse(message proto.Message) error {
	if message == nil || !message.ProtoReflect().IsValid() {
		return ErrInvalidResponse
	}
	if err := protovalidate.Validate(message); err != nil {
		return ErrInvalidResponse
	}
	return nil
}

func cursorToken(cursor *contractsv1.Cursor) string {
	if cursor == nil {
		return ""
	}
	return cursor.Token
}

func nextCursor(token string) *contractsv1.Cursor {
	if token == "" {
		return nil
	}
	return &contractsv1.Cursor{Token: token}
}
