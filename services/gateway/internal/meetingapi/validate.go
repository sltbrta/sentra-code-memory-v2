package meetingapi

import (
	"buf.build/go/protovalidate"
	contractsv1 "github.com/sltbrta/sentra-code-memory-v2/packages/contracts/gen/go/ouroboros/contracts/v1"
	"github.com/sltbrta/sentra-code-memory-v2/services/gateway/internal/localauthority"
	shared "github.com/sltbrta/sentra-code-memory-v2/services/internal/contracts"
	"google.golang.org/protobuf/proto"
)

func crossCheckPeer(peer localauthority.PeerContext, caller *contractsv1.UntrustedMeetingCaller) (Principal, error) {
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

func validateRequest(message proto.Message) error {
	if message == nil || !message.ProtoReflect().IsValid() {
		return ErrInvalidRequest
	}
	if err := protovalidate.Validate(message); err != nil {
		return ErrInvalidRequest
	}
	return nil
}

func validateResponse(message proto.Message) error {
	if message == nil || !message.ProtoReflect().IsValid() {
		return ErrInvalidResponse
	}
	if err := protovalidate.Validate(message); err != nil {
		return ErrInvalidResponse
	}
	return nil
}

func isLowerHexSHA256(value string) bool {
	if len(value) != 64 {
		return false
	}
	for _, character := range value {
		if (character < '0' || character > '9') && (character < 'a' || character > 'f') {
			return false
		}
	}
	return true
}
