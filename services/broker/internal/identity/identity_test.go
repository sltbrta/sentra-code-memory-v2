package identity

import (
	"errors"
	"testing"

	"github.com/sltbrta/sentra-code-memory-v2/services/internal/contracts"
)

func TestMapperAuthenticatesPeerAndCrossChecksBody(t *testing.T) {
	principal := contracts.Identifier{Namespace: "principal", Value: "owner"}
	tenant := contracts.Identifier{Namespace: "tenant", Value: "personal"}
	mapper, err := NewMapper(501, principal, tenant)
	if err != nil {
		t.Fatal(err)
	}
	fact, err := mapper.Map(contracts.PeerCredentials{UID: 501, GID: 20, PID: 99}, contracts.Identifier{Namespace: "session", Value: "s1"})
	if err != nil {
		t.Fatal(err)
	}
	if err := CrossCheckCommand(fact, principal, tenant, fact.Session); err != nil {
		t.Fatal(err)
	}
	if err := CrossCheckCommand(fact, contracts.Identifier{Namespace: "principal", Value: "other"}, tenant, fact.Session); !errors.Is(err, ErrDenied) {
		t.Fatalf("principal mismatch error = %v", err)
	}
	wrongSession := contracts.Identifier{Namespace: "session", Value: "other"}
	if err := CrossCheckCommand(fact, principal, tenant, wrongSession); !errors.Is(err, ErrDenied) {
		t.Fatalf("command session mismatch error = %v", err)
	}
	if err := CrossCheckStatus(fact, wrongSession); !errors.Is(err, ErrDenied) {
		t.Fatalf("status session mismatch error = %v", err)
	}
	if err := CrossCheckStatus(fact, fact.Session); err != nil {
		t.Fatal(err)
	}
	if _, err := mapper.Map(contracts.PeerCredentials{UID: 502, PID: 99}, contracts.Identifier{Namespace: "session", Value: "s1"}); !errors.Is(err, ErrDenied) {
		t.Fatalf("wrong uid error = %v", err)
	}
}

func TestIdentityRejectsMissingAndMalformedValues(t *testing.T) {
	if _, err := NewMapper(501, contracts.Identifier{}, contracts.Identifier{Namespace: "tenant", Value: "t"}); !errors.Is(err, ErrDenied) {
		t.Fatalf("missing principal error = %v", err)
	}
}
