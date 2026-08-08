package factoryapi

import (
	contractsv1 "github.com/sltbrta/sentra-code-memory-v2/packages/contracts/gen/go/ouroboros/contracts/v1"
	shared "github.com/sltbrta/sentra-code-memory-v2/services/internal/contracts"
)

// deniedCode is the single static public error code every unknown,
// unauthorized, stale, or revoked run shares; it discloses no existence
// detail. Admission conflicts — stale base, stale lease or fence, revoked
// grant, conflicting idempotency-key reuse — collapse to the same shape.
const deniedCode = "not_found_or_denied"

// Identifier namespaces mirror the Stage 03/04 adapter conventions.
const (
	namespaceReceipt   = "receipt"
	namespaceOperation = "operation"
)

// Operation identifiers bound into every emitted receipt.
const (
	operationAdmit     = "factory.admit"
	operationPlan      = "factory.plan"
	operationCandidate = "factory.candidate"
	operationReview    = "factory.review"
	operationCancel    = "factory.cancel"
)

func staticPublicError() *contractsv1.PublicError {
	return &contractsv1.PublicError{Code: deniedCode}
}

func sessionCausal(identity shared.MappedIdentityFact) *contractsv1.CausalContext {
	return &contractsv1.CausalContext{
		CorrelationId: &contractsv1.Identifier{Namespace: "correlation", Value: identity.Session.Value},
		CausationId:   &contractsv1.Identifier{Namespace: "session", Value: identity.Session.Value},
		TraceId:       &contractsv1.Identifier{Namespace: "trace", Value: identity.Session.Value},
	}
}

// isLowerHexSHA256 reports whether value is a canonical SHA-256 hex string:
// exactly 64 lowercase hexadecimal characters, as the frozen Digest shapes
// require.
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
