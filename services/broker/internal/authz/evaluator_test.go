package authz

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/sltbrta/sentra-code-memory-v2/services/internal/contracts"
)

type fixture struct {
	Tuples []string `json:"tuples"`
	Checks []struct {
		ID          string `json:"id"`
		User        string `json:"user"`
		Relation    string `json:"relation"`
		Object      string `json:"object"`
		Allowed     bool   `json:"allowed"`
		AfterRemove string `json:"after_remove"`
	} `json:"checks"`
}

func TestEvaluatorExecutesPinnedOpenFGAFixtures(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "..", "..", "deploy", "openfga", "local", "fixtures.json"))
	if err != nil {
		t.Skipf("optional OpenFGA fixture is not present in this standalone checkout: %v", err)
	}
	var cases fixture
	if err := json.Unmarshal(data, &cases); err != nil {
		t.Fatal(err)
	}
	evaluator := NewEvaluator()
	for _, raw := range cases.Tuples {
		tuple, err := ParseTuple(raw)
		if err != nil {
			t.Fatal(err)
		}
		if err := evaluator.Write(tuple); err != nil {
			t.Fatal(err)
		}
	}
	for _, check := range cases.Checks {
		if check.AfterRemove != "" {
			tuple, err := ParseTuple(check.AfterRemove)
			if err != nil {
				t.Fatal(err)
			}
			tenant, _, err := evaluator.Delete(tuple)
			if err != nil {
				t.Fatal(err)
			}
			if tenant != "personal" {
				t.Fatalf("derived tenant = %q", tenant)
			}
		}
		principal := check.User[len("user:"):]
		resource := check.Object[len("evidence:"):]
		action := map[string]string{"reader": "artifact.read", "admittor": "artifact.admit", "deleter": "artifact.delete"}[check.Relation]
		decision, err := evaluator.Check(context.Background(), identityFact(principal), contracts.PolicyRequest{
			Action: action, Resource: contracts.Identifier{Namespace: "evidence", Value: resource}, RevocationEpoch: evaluator.epochs["personal"],
		})
		if err != nil {
			t.Fatal(err)
		}
		if decision.Allowed != check.Allowed {
			t.Fatalf("%s allowed = %v, want %v", check.ID, decision.Allowed, check.Allowed)
		}
	}
}

func TestDeleteDerivesTenantAtomicallyAndRejectsAmbiguity(t *testing.T) {
	evaluator := NewEvaluator()
	for _, raw := range []string{"brain:b#tenant@tenant:t", "brain:b#viewer@user:p"} {
		tuple, err := ParseTuple(raw)
		if err != nil {
			t.Fatal(err)
		}
		if err := evaluator.Write(tuple); err != nil {
			t.Fatal(err)
		}
	}
	viewer, _ := ParseTuple("brain:b#viewer@user:p")
	tenant, epoch, err := evaluator.Delete(viewer)
	if err != nil || tenant != "t" || epoch != 1 {
		t.Fatalf("delete = %q, %d, %v", tenant, epoch, err)
	}
	if _, _, err := evaluator.Delete(viewer); !errors.Is(err, ErrMalformedTuple) {
		t.Fatalf("missing tuple error = %v", err)
	}
	ambiguous := NewEvaluator()
	for _, raw := range []string{"brain:b#tenant@tenant:t1", "brain:b#tenant@tenant:t2", "brain:b#viewer@user:p"} {
		tuple, _ := ParseTuple(raw)
		if err := ambiguous.Write(tuple); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := ambiguous.Delete(viewer); !errors.Is(err, ErrMalformedTuple) {
		t.Fatalf("ambiguous tenant error = %v", err)
	}
}

func TestEvaluatorDeniesStaleEpochMalformedAndCrossTenant(t *testing.T) {
	evaluator := NewEvaluator()
	for _, raw := range []string{
		"brain:b#tenant@tenant:t", "brain:b#owner@user:p", "evidence:a#brain@brain:b",
	} {
		tuple, err := ParseTuple(raw)
		if err != nil {
			t.Fatal(err)
		}
		if err := evaluator.Write(tuple); err != nil {
			t.Fatal(err)
		}
	}
	if err := evaluator.SetEpoch("t", 2); err != nil {
		t.Fatal(err)
	}
	request := contracts.PolicyRequest{Action: "artifact.read", Resource: contracts.Identifier{Namespace: "evidence", Value: "a"}, RevocationEpoch: 1}
	decision, err := evaluator.Check(context.Background(), identityFactFor("p", "t"), request)
	if err != nil || decision.Allowed || decision.Receipt.ReasonCode != "not_found_or_denied" {
		t.Fatalf("stale decision = %#v, %v", decision, err)
	}
	request.RevocationEpoch = 2
	decision, err = evaluator.Check(context.Background(), identityFactFor("p", "other"), request)
	if err != nil || decision.Allowed {
		t.Fatalf("cross-tenant decision = %#v, %v", decision, err)
	}
}

func TestEvaluatorChecksCurrentSourceRelationships(t *testing.T) {
	evaluator := NewEvaluator()
	for _, raw := range []string{
		"brain:b#tenant@tenant:t", "brain:b#owner@user:owner", "brain:b#viewer@user:viewer",
	} {
		tuple, err := ParseTuple(raw)
		if err != nil {
			t.Fatal(err)
		}
		if err := evaluator.Write(tuple); err != nil {
			t.Fatal(err)
		}
	}
	brain := contracts.Identifier{Namespace: "brain", Value: "b"}
	for _, action := range []string{"source.add", "source.reconcile", "source.revoke"} {
		decision, err := evaluator.CheckSource(context.Background(), identityFactFor("owner", "t"), action, brain)
		if err != nil || !decision.Allowed {
			t.Fatalf("owner %s = %#v, %v", action, decision, err)
		}
		decision, err = evaluator.CheckSource(context.Background(), identityFactFor("viewer", "t"), action, brain)
		if err != nil || decision.Allowed {
			t.Fatalf("viewer %s = %#v, %v", action, decision, err)
		}
	}
	for _, action := range []string{"factory.admit", "factory.cancel", "file.read", "file.write"} {
		decision, err := evaluator.CheckSource(context.Background(), identityFactFor("owner", "t"), action, brain)
		if err != nil || !decision.Allowed {
			t.Fatalf("owner %s = %#v, %v", action, decision, err)
		}
		decision, err = evaluator.CheckSource(context.Background(), identityFactFor("viewer", "t"), action, brain)
		if err != nil || decision.Allowed {
			t.Fatalf("viewer %s = %#v, %v", action, decision, err)
		}
	}
	for _, action := range []string{"source.status", "source.search", "query", "hydrate", "emit"} {
		decision, err := evaluator.CheckSource(context.Background(), identityFactFor("viewer", "t"), action, brain)
		if err != nil || !decision.Allowed {
			t.Fatalf("viewer %s = %#v, %v", action, decision, err)
		}
	}
	for _, identity := range []contracts.MappedIdentityFact{
		identityFactFor("owner", "other"),
		{Principal: contracts.Identifier{Namespace: "principal"}, Tenant: contracts.Identifier{Namespace: "tenant", Value: "t"}},
	} {
		decision, err := evaluator.CheckSource(context.Background(), identity, "source.status", brain)
		if err != nil || decision.Allowed || decision.Receipt.ReasonCode != "not_found_or_denied" {
			t.Fatalf("denial = %#v, %v", decision, err)
		}
	}
	viewer, _ := ParseTuple("brain:b#viewer@user:viewer")
	if _, _, err := evaluator.Delete(viewer); err != nil {
		t.Fatal(err)
	}
	decision, err := evaluator.CheckSource(context.Background(), identityFactFor("viewer", "t"), "source.search", brain)
	if err != nil || decision.Allowed {
		t.Fatalf("revoked relationship = %#v, %v", decision, err)
	}
}

func identityFact(principal string) contracts.MappedIdentityFact {
	return identityFactFor(principal, "personal")
}

func identityFactFor(principal, tenant string) contracts.MappedIdentityFact {
	return contracts.MappedIdentityFact{
		Principal: contracts.Identifier{Namespace: "principal", Value: principal},
		Tenant:    contracts.Identifier{Namespace: "tenant", Value: tenant},
		Session:   contracts.Identifier{Namespace: "session", Value: "s"},
	}
}
