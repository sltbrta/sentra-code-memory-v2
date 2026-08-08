package ontology_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/ontology"
)

func TestLoadPredicatePolicyEmbed(t *testing.T) {
	p := ontology.LoadPredicatePolicy("")
	if !p.IsMultiValued("tags") || !p.IsMultiValued("aliases") {
		t.Fatalf("embed pack multi-valued: %+v source=%s", p.MultiValued, p.Source)
	}
	if p.IsMultiValued("color") {
		t.Fatal("color must not be multi-valued by default")
	}
	if p.Source != "embed" && p.Source != "default" {
		t.Fatalf("source=%s", p.Source)
	}
}

func TestLoadPredicatePolicyMissingFallsBack(t *testing.T) {
	p := ontology.LoadPredicatePolicy(filepath.Join(t.TempDir(), "nope.yaml"))
	if !p.IsMultiValued("tags") {
		t.Fatalf("fallback default must include tags: %+v", p)
	}
}

func TestLoadPredicatePolicyFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "pack.yaml")
	body := "version: test.v1\nmulti_valued_predicates:\n  - custom_pred\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	p := ontology.LoadPredicatePolicy(path)
	if p.Source != "file" {
		t.Fatalf("source=%s", p.Source)
	}
	if !p.IsMultiValued("custom_pred") {
		t.Fatalf("custom_pred: %+v", p.MultiValued)
	}
	if p.IsMultiValued("tags") {
		// file overrides fully — tags not listed
		t.Log("file pack without tags is ok")
	}
}

func TestActivePredicatePolicy(t *testing.T) {
	ontology.ResetPredicatePolicyForTest()
	defer ontology.ResetPredicatePolicyForTest()
	if !ontology.IsMultiValuedPredicate("aliases") {
		t.Fatal("active policy should load aliases")
	}
	ontology.SetActivePredicatePolicy(ontology.PredicatePolicy{
		MultiValued: map[string]struct{}{"only_me": {}},
	})
	if !ontology.IsMultiValuedPredicate("only_me") {
		t.Fatal("set policy")
	}
	if ontology.IsMultiValuedPredicate("tags") {
		t.Fatal("tags not in custom policy")
	}
}
