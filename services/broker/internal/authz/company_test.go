package authz

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/sltbrta/sentra-code-memory-v2/services/internal/contracts"
)

type companyFixture struct {
	Tuples []string `json:"tuples"`
	Checks []struct {
		ID          string `json:"id"`
		User        string `json:"user"`
		Relation    string `json:"relation"`
		Object      string `json:"object"`
		Tenant      string `json:"tenant"`
		Allowed     bool   `json:"allowed"`
		AfterRemove string `json:"after_remove"`
	} `json:"checks"`
	Residuals struct {
		Issue int    `json:"issue"`
		Def   string `json:"def"`
		Claim string `json:"claim"`
	} `json:"residuals"`
}

func TestCompanyFixturesPartialOpenFGAPath(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "..", "..", "deploy", "openfga", "local", "fixtures_company.json"))
	if err != nil {
		// Bazel runfiles
		data, err = os.ReadFile("deploy/openfga/local/fixtures_company.json")
	}
	if err != nil {
		if root := os.Getenv("TEST_SRCDIR"); root != "" {
			workspace := os.Getenv("TEST_WORKSPACE")
			if workspace == "" {
				workspace = "_main"
			}
			data, err = os.ReadFile(filepath.Join(root, workspace, "deploy", "openfga", "local", "fixtures_company.json"))
		}
	}
	if err != nil {
		t.Skipf("optional OpenFGA company fixture is not present in this standalone checkout: %v", err)
	}
	var cases companyFixture
	if err := json.Unmarshal(data, &cases); err != nil {
		t.Fatal(err)
	}
	if cases.Residuals.Issue != 72 || cases.Residuals.Def != "DEF-015" || cases.Residuals.Claim != "partial" {
		t.Fatalf("residuals = %+v", cases.Residuals)
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
			if _, _, err := evaluator.Delete(tuple); err != nil {
				t.Fatal(err)
			}
		}
		principal := check.User[len("user:"):]
		resource := check.Object[len("evidence:"):]
		tenant := check.Tenant
		if tenant == "" {
			tenant = "company-acme"
		}
		action := map[string]string{"reader": "artifact.read", "admittor": "artifact.admit", "deleter": "artifact.delete"}[check.Relation]
		epoch, _ := evaluator.Epoch(tenant)
		decision, err := evaluator.Check(context.Background(), identityFactFor(principal, tenant), contracts.PolicyRequest{
			Action: action, Resource: contracts.Identifier{Namespace: "evidence", Value: resource}, RevocationEpoch: epoch,
		})
		if err != nil {
			t.Fatal(err)
		}
		if decision.Allowed != check.Allowed {
			t.Fatalf("%s allowed = %v, want %v", check.ID, decision.Allowed, check.Allowed)
		}
	}
}
