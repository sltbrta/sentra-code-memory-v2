package ontology_test

import (
	"fmt"
	"testing"

	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/ontology"
)

// PPR collected scores from a map and sorted on score alone. Exact ties are the
// normal case over a symmetric co-occurrence graph -- not an edge case -- so the
// surviving order was whatever Go's randomised map iteration produced, and
// identical inputs returned different candidate sets between processes.

// symmetricStar is the canonical tie generator: every leaf is equidistant from
// the hub, so every leaf scores identically.
// PPR builds its adjacency from the DocumentSrc/DocumentDst pair, so the
// fixture has to populate those rather than the entity ids.
func symmetricStar(leaves int) ontology.Graph {
	g := ontology.Graph{}
	for i := 0; i < leaves; i++ {
		leaf := fmt.Sprintf("leaf-%02d", i)
		g.Edges = append(g.Edges,
			ontology.Edge{DocumentSrc: "hub", DocumentDst: leaf, Weight: 1},
			ontology.Edge{DocumentSrc: leaf, DocumentDst: "hub", Weight: 1},
		)
	}
	return g
}

func TestPersonalizedPageRankIsStableAcrossRuns(t *testing.T) {
	g := symmetricStar(12)
	seeds := []string{"hub"}

	first := ontology.PPR(g, seeds, 15, 0.85, 6)
	if len(first) == 0 {
		t.Fatal("no results; the fixture does not exercise ranking")
	}
	for run := 2; run <= 30; run++ {
		got := ontology.PPR(g, seeds, 15, 0.85, 6)
		if len(got) != len(first) {
			t.Fatalf("run %d returned %d results, run 1 returned %d", run, len(got), len(first))
		}
		for i := range got {
			if got[i] != first[i] {
				t.Fatalf("run %d differs at %d: %q vs %q\nrun 1: %v\nrun %d: %v",
					run, i, got[i], first[i], first, run, got)
			}
		}
	}
}
