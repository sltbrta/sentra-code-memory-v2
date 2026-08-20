package memory_test

import (
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

func TestPersonalizedPageRankIsDeterministic(t *testing.T) {
	edges := map[string][]string{"a": {"b", "c"}, "b": {"a"}, "c": {"a"}}
	seeds := map[string]float64{"a": 0.34, "b": 0.33, "c": 0.33}

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
