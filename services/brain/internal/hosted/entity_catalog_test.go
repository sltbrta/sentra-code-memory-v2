package hosted

import (
	"context"
	"strings"
	"testing"
)

func TestWantsExhaustiveAgentic(t *testing.T) {
	if !wantsExhaustiveAgentic("List all customers with retention exceptions", "completeness", QueryPlan{Completeness: true}) {
		t.Fatal("completeness")
	}
	if !wantsExhaustiveAgentic("What are the company-wide themes this quarter?", "semantic", QueryPlan{}) {
		t.Fatal("agg regex")
	}
	if wantsExhaustiveAgentic("What is the RPO?", "basic", QueryPlan{}) {
		t.Fatal("basic fact should not exhaust")
	}
}

func TestExhaustiveFacetsFromIdentifiers(t *testing.T) {
	ps := []Passage{
		{DocumentID: "d1", Text: "Acme Corp retention window is 12 months under Policy Handbook."},
	}
	f := exhaustiveFacets(nil, context.Background(), "INC-1234 retention for Acme", ps, 8)
	if len(f) == 0 {
		t.Fatal("want facets from identifiers / pack")
	}
	joined := strings.Join(f, " ")
	if !strings.Contains(joined, "INC") && !strings.Contains(joined, "Acme") && !strings.Contains(strings.ToLower(joined), "retention") {
		t.Fatalf("facets=%v", f)
	}
}
