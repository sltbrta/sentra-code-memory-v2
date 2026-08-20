package memory_test

import (
	"fmt"
	"math"
	"testing"

	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/memory"
)

// PPR redistributed dangling mass only for nodes that were keys of the
// adjacency map. A seed contributed by seedScores but absent from the
// adjacency -- the ordinary case for a retrieved document with no
// co-occurrence edges -- kept its rank and never propagated it, so
// damping*rank vanished on every iteration and every score came out deflated
// by an amount that depended on the query.

func TestPersonalizedPageRankConservesMassWithDanglingSeeds(t *testing.T) {
	// Two connected documents plus an isolated seed with no edges at all.
	edges := map[string][]string{
		"a": {"b"},
		"b": {"a"},
	}
	seeds := map[string]float64{"a": 0.4, "b": 0.3, "isolated": 0.3}

	scores := memory.PersonalizedPageRank(seeds, edges, 0.85, 30)

	total := 0.0
	for _, v := range scores {
		total += v
	}
	if math.Abs(total-1.0) > 1e-6 {
		t.Fatalf("total mass = %.9f, want 1.0: dangling rank leaked out of the graph (scores=%v)", total, scores)
	}
}

// A fresh-eyes review pointed out that the first version of this test used a
// three-node graph whose sums are exactly representable in float64, so it
// passed regardless of accumulation order and proved nothing. Float addition is
// not associative; the defect only shows on a graph large enough for the
// rounding to differ.
func TestPersonalizedPageRankIsDeterministic(t *testing.T) {
	// The contributions must differ from each other, or float addition is
	// order-independent and the test proves nothing however large the graph is
	// -- which is what the first two versions of this fixture got wrong. Seeds
	// are irrational-ish and unequal, and out-degrees vary, so each node
	// contributes a distinct share.
	edges := map[string][]string{}
	seeds := map[string]float64{}
	const fanIn = 400
	for i := 0; i < fanIn; i++ {
		node := fmt.Sprintf("n%03d", i)
		edges[node] = []string{"hub"}
		if i%3 == 0 {
			edges[node] = append(edges[node], fmt.Sprintf("n%03d", (i+7)%fanIn))
		}
		edges["hub"] = append(edges["hub"], node)
		seeds[node] = math.Sqrt(float64(i)+1) / float64(fanIn)
	}

	first := memory.PersonalizedPageRank(seeds, edges, 0.85, 20)
	for run := 2; run <= 20; run++ {
		got := memory.PersonalizedPageRank(seeds, edges, 0.85, 20)
		for id, want := range first {
			if got[id] != want {
				t.Fatalf("run %d: score for %q = %v, run 1 = %v (float accumulation order is not stable)",
					run, id, got[id], want)
			}
		}
	}
}
