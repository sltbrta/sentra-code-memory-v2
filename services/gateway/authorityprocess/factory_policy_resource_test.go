package authorityprocess

import (
	"context"
	"testing"

	broker "github.com/sltbrta/sentra-code-memory-v2/services/broker/localauthority"
	shared "github.com/sltbrta/sentra-code-memory-v2/services/internal/contracts"
)

// A-009. factoryPolicyAdapter.Check discarded request.Resource and always
// authorised against the fixed adapter.brain, so policy was evaluated
// per-tenant rather than per-resource: a grant on one brain authorised every
// brain in the tenant, and the broker's containsResource check downstream
// compared a value against itself.
//
// The fix was verified by reading only. Reverting it left the suite green,
// because nothing constructed this adapter with a resource that differed from
// its configured brain -- which is the only shape in which the bug is visible.

// twoBrainPolicy grants the caller ownership of brain "granted" and nothing at
// all on brain "other", which exists in the same tenant.
func twoBrainPolicy(t *testing.T) *broker.Broker {
	t.Helper()
	policy, err := broker.New(broker.Config{
		UID: 501, Principal: broker.Identifier{Namespace: "principal", Value: "p"},
		Tenant:  broker.Identifier{Namespace: "tenant", Value: "t"},
		Session: broker.Identifier{Namespace: "session", Value: "s"},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, relationship := range []string{
		"brain:granted#tenant@tenant:t", "brain:granted#owner@user:p",
		// "other" is registered to the same tenant but grants nobody anything.
		"brain:other#tenant@tenant:t",
	} {
		if err := policy.AddRelationship(relationship); err != nil {
			t.Fatal(err)
		}
	}
	return policy
}

func factoryResourceCaller() shared.MappedIdentityFact {
	return shared.MappedIdentityFact{
		Principal: shared.Identifier{Namespace: "principal", Value: "p"},
		Tenant:    shared.Identifier{Namespace: "tenant", Value: "t"},
		Session:   shared.Identifier{Namespace: "session", Value: "s"},
	}
}

func TestFactoryPolicyDeniesAResourceTheGrantDoesNotName(t *testing.T) {
	t.Parallel()
	adapter := factoryPolicyAdapter{
		broker: twoBrainPolicy(t),
		brain:  broker.Identifier{Namespace: "brain", Value: "granted"},
	}
	decision, err := adapter.Check(context.Background(), factoryResourceCaller(), shared.PolicyRequest{
		Action:   "factory.admit",
		Resource: shared.Identifier{Namespace: "brain", Value: "other"},
	})
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if decision.Allowed {
		t.Fatal("a grant on brain \"granted\" authorised brain \"other\": " +
			"request.Resource is being discarded and the configured brain substituted")
	}
}

func TestFactoryPolicyAllowsTheResourceTheGrantNames(t *testing.T) {
	t.Parallel()
	adapter := factoryPolicyAdapter{
		broker: twoBrainPolicy(t),
		brain:  broker.Identifier{Namespace: "brain", Value: "granted"},
	}
	decision, err := adapter.Check(context.Background(), factoryResourceCaller(), shared.PolicyRequest{
		Action:   "factory.admit",
		Resource: shared.Identifier{Namespace: "brain", Value: "granted"},
	})
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if !decision.Allowed {
		t.Fatal("the granted resource must still be authorised")
	}
}

// TestFactoryPolicyFallsBackToTheConfiguredBrain pins the compatibility half:
// a request that names no resource must still evaluate against the brain the
// adapter was composed with, rather than against an empty identifier that
// would deny everything.
func TestFactoryPolicyFallsBackToTheConfiguredBrain(t *testing.T) {
	t.Parallel()
	adapter := factoryPolicyAdapter{
		broker: twoBrainPolicy(t),
		brain:  broker.Identifier{Namespace: "brain", Value: "granted"},
	}
	decision, err := adapter.Check(context.Background(), factoryResourceCaller(), shared.PolicyRequest{
		Action: "factory.admit",
	})
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if !decision.Allowed {
		t.Fatal("an unnamed resource must fall back to the configured brain")
	}
}
