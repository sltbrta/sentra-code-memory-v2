package ontology

import "testing"

func TestValidateAndNeighbors(t *testing.T) {
	t.Parallel()
	g := Graph{
		GenerationID: "gen-1",
		Edges: []Edge{
			{DocumentSrc: "a", DocumentDst: "b", Rel: RelCoProject, Weight: 1},
			{DocumentSrc: "b", DocumentDst: "c", Rel: RelCites, Weight: 0.8},
		},
	}
	if err := ValidateGraph(g); err != nil {
		t.Fatalf("validate: %v", err)
	}
	n := Neighbors(g, []string{"a"}, 10)
	if len(n) == 0 || n[0] != "b" {
		t.Fatalf("neighbors = %v", n)
	}
}

func TestPPRPrefersConnected(t *testing.T) {
	t.Parallel()
	g := Graph{
		GenerationID: "gen-1",
		Edges: []Edge{
			{DocumentSrc: "seed", DocumentDst: "near", Rel: RelCoProject, Weight: 1},
			{DocumentSrc: "near", DocumentDst: "far", Rel: RelCites, Weight: 1},
			{DocumentSrc: "island", DocumentDst: "other", Rel: RelMentions, Weight: 1},
		},
	}
	ranked := PPR(g, []string{"seed"}, 15, 0.85, 5)
	if len(ranked) == 0 {
		t.Fatal("empty ppr")
	}
	// seed or near should outrank island
	foundNear := false
	for _, id := range ranked[:min(3, len(ranked))] {
		if id == "near" || id == "seed" {
			foundNear = true
		}
		if id == "island" && ranked[0] == "island" {
			t.Fatalf("island should not rank first: %v", ranked)
		}
	}
	if !foundNear {
		t.Fatalf("expected seed/near in top: %v", ranked)
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
