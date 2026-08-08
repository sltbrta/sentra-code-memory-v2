package authorityprocess

import (
	"time"

	contractsv1 "github.com/sltbrta/sentra-code-memory-v2/packages/contracts/gen/go/ouroboros/contracts/v1"
	brain "github.com/sltbrta/sentra-code-memory-v2/services/brain/localauthority"
	shared "github.com/sltbrta/sentra-code-memory-v2/services/internal/contracts"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func receipt(value shared.Receipt, recordedAt int64, config brain.Digest, causal *contractsv1.CausalContext) *contractsv1.Receipt {
	return &contractsv1.Receipt{
		ReceiptId: &contractsv1.Identifier{Namespace: "receipt", Value: value.OperationID.Value},
		Status:    receiptStatus(value.Status), ReasonCode: value.ReasonCode,
		OperationId: identifierProto(value.OperationID), Causal: causal,
		RecordedAt:          timestamppb.New(time.UnixMilli(recordedAt)),
		ConfigurationDigest: protoDigest(config),
	}
}

func receiptStatus(value string) contractsv1.ReceiptStatus {
	switch value {
	case "accepted":
		return contractsv1.ReceiptStatus_RECEIPT_STATUS_ACCEPTED
	case "rejected":
		return contractsv1.ReceiptStatus_RECEIPT_STATUS_REJECTED
	case "deferred":
		return contractsv1.ReceiptStatus_RECEIPT_STATUS_DEFERRED
	case "partial":
		return contractsv1.ReceiptStatus_RECEIPT_STATUS_PARTIAL
	case "completed":
		return contractsv1.ReceiptStatus_RECEIPT_STATUS_COMPLETED
	default:
		return contractsv1.ReceiptStatus_RECEIPT_STATUS_UNSPECIFIED
	}
}

func authorizationReceipt(request *contractsv1.ExecuteAuthorityCommandRequest, result brain.Result) *contractsv1.AuthorizationReceipt {
	value := receipt(result.Receipt, result.RecordedAtMilli, result.ConfigurationDigest, request.Command.Causal)
	value.ReceiptId = &contractsv1.Identifier{Namespace: "receipt", Value: "authorization-" + result.Receipt.OperationID.Value}
	value.ReasonCode = result.Authorization.ReasonCode
	return &contractsv1.AuthorizationReceipt{
		Receipt: value, GrantId: request.Grant.GrantId,
		Action: request.Command.CommandType, Resource: &contractsv1.Identifier{Namespace: "evidence", Value: result.Artifact.ID.Value},
		AclEpoch: result.Authorization.RevocationEpoch,
	}
}

func sessionCausal(identity shared.MappedIdentityFact) *contractsv1.CausalContext {
	value := identity.Session.Value
	return &contractsv1.CausalContext{
		CorrelationId: &contractsv1.Identifier{Namespace: "correlation", Value: value},
		CausationId:   &contractsv1.Identifier{Namespace: "session", Value: value},
		TraceId:       &contractsv1.Identifier{Namespace: "trace", Value: value},
	}
}

func principal(identity shared.MappedIdentityFact) *contractsv1.AuthenticatedPrincipalRef {
	return &contractsv1.AuthenticatedPrincipalRef{
		PrincipalId: identifierProto(identity.Principal), TenantId: identifierProto(identity.Tenant),
		SessionId: identifierProto(identity.Session),
	}
}

func samePrincipal(value *contractsv1.AuthenticatedPrincipalRef, identity shared.MappedIdentityFact) bool {
	return value != nil && sameIdentifier(value.PrincipalId, identity.Principal) &&
		sameIdentifier(value.TenantId, identity.Tenant) && sameIdentifier(value.SessionId, identity.Session)
}

func sameIdentifier(value *contractsv1.Identifier, expected shared.Identifier) bool {
	return value != nil && value.Namespace == expected.Namespace && value.Value == expected.Value
}

func identifier(value *contractsv1.Identifier) shared.Identifier {
	if value == nil {
		return shared.Identifier{}
	}
	return shared.Identifier{Namespace: value.Namespace, Value: value.Value}
}

func identifierProto(value shared.Identifier) *contractsv1.Identifier {
	return &contractsv1.Identifier{Namespace: value.Namespace, Value: value.Value}
}

func digest(value *contractsv1.Digest) shared.Digest {
	if value == nil {
		return shared.Digest{}
	}
	return shared.Digest{Algorithm: value.Algorithm, Hex: value.Hex}
}

func protoDigest(value shared.Digest) *contractsv1.Digest {
	return &contractsv1.Digest{Algorithm: value.Algorithm, Hex: value.Hex}
}
