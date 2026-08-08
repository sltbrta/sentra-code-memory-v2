package tracer001

import (
	"buf.build/go/protovalidate"
	contractsv1 "github.com/sltbrta/sentra-code-memory-v2/packages/contracts/gen/go/ouroboros/contracts/v1"
	"github.com/sltbrta/sentra-code-memory-v2/services/gateway/internal/localauthority"
	shared "github.com/sltbrta/sentra-code-memory-v2/services/internal/contracts"
	"google.golang.org/protobuf/proto"
)

// crossCheckPeer derives the trusted principal exclusively from the peer and
// rejects any body identity that disagrees with it before any port call.
func crossCheckPeer(
	peer localauthority.PeerContext,
	caller *contractsv1.AuthenticatedPrincipalRef,
	session *contractsv1.Identifier,
) (Principal, error) {
	identity := peer.Identity
	if !validMappedIdentity(identity) || caller == nil {
		return Principal{}, ErrRequestDenied
	}
	if !sameIdentifier(caller.PrincipalId, identity.Principal) ||
		!sameIdentifier(caller.TenantId, identity.Tenant) ||
		!sameIdentifier(caller.SessionId, identity.Session) {
		return Principal{}, ErrRequestDenied
	}
	if session != nil && !sameIdentifier(session, identity.Session) {
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

func validateMessage(message proto.Message) error {
	if message == nil || !message.ProtoReflect().IsValid() {
		return ErrInvalidRequest
	}
	if err := protovalidate.Validate(message); err != nil {
		return ErrInvalidRequest
	}
	return nil
}

func validateResponseMessage(message proto.Message) error {
	if message == nil || !message.ProtoReflect().IsValid() {
		return ErrInvalidResponse
	}
	if err := protovalidate.Validate(message); err != nil {
		return ErrInvalidResponse
	}
	return nil
}

// validateRequestShape enforces the minimal per-step field requirements before
// nested contract validation and port invocation.
func validateRequestShape(step Step, request PathRequest) error {
	if request.Caller == nil || request.Caller.PrincipalId == nil ||
		request.Caller.TenantId == nil || request.Caller.SessionId == nil {
		return ErrInvalidRequest
	}
	if err := validateMessage(request.Caller); err != nil {
		return err
	}
	switch step {
	case StepSession:
		if request.IdempotencyKey == "" || len(request.IdempotencyKey) > 512 {
			return ErrInvalidRequest
		}
		return nil
	case StepIngest:
		if request.RunID == nil || request.ManifestDigest == nil || request.IdempotencyKey == "" {
			return ErrInvalidRequest
		}
		if err := validateMessage(request.RunID); err != nil {
			return err
		}
		return validateMessage(request.ManifestDigest)
	case StepAsk, StepOutcome:
		if request.RunID == nil || request.IdempotencyKey == "" || request.QueryText == "" {
			return ErrInvalidRequest
		}
		if len(request.QueryText) > 512 {
			return ErrInvalidRequest
		}
		if err := validateMessage(request.RunID); err != nil {
			return err
		}
		if step == StepAsk {
			if request.SourceID == nil || request.GenerationID == nil {
				return ErrInvalidRequest
			}
			if err := validateMessage(request.SourceID); err != nil {
				return err
			}
			if err := validateMessage(request.GenerationID); err != nil {
				return err
			}
		}
		return nil
	case StepIntent:
		if request.RunID == nil || request.IdempotencyKey == "" || request.BaseGitOID == "" ||
			request.ScopeDigest == nil {
			return ErrInvalidRequest
		}
		if err := validateMessage(request.RunID); err != nil {
			return err
		}
		return validateMessage(request.ScopeDigest)
	case StepPlan, StepReview:
		if request.RunID == nil {
			return ErrInvalidRequest
		}
		return validateMessage(request.RunID)
	case StepDraftPR:
		if request.RunID == nil || request.IdempotencyKey == "" ||
			!isLowerHexSHA256(request.EffectApprovalHex) ||
			!isLowerHexSHA256(request.ChangeSetDigestHex) {
			return ErrInvalidRequest
		}
		return validateMessage(request.RunID)
	default:
		return ErrInvalidRequest
	}
}
