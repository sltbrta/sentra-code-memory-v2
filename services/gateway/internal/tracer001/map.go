package tracer001

import (
	contractsv1 "github.com/sltbrta/sentra-code-memory-v2/packages/contracts/gen/go/ouroboros/contracts/v1"
	shared "github.com/sltbrta/sentra-code-memory-v2/services/internal/contracts"
)

// deniedCode is the single static public error every unknown, unauthorized,
// stale, or revoked scope shares; it discloses no existence detail.
const deniedCode = "not_found_or_denied"

const (
	namespaceReceipt   = "receipt"
	namespaceOperation = "operation"
)

func operationID(step Step) string {
	return "tracer." + string(step)
}

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
