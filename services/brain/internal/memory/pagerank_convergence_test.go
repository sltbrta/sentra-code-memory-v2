package memory

import (
	"fmt"
	"math"
	"testing"
	"time"
)

// N-008. GlobalPageRank ran its full iteration budget unconditionally, with no
// convergence test: on a large graph twenty passes returned a prior that had
// not settled, and on a small one every pass after the answer stopped moving
// was burned for nothing, with no way to tell which had happened. The same
// loop also accumulated floats in Go map order, so the stored vector -- and
// therefore memory.json -- differed bit-for-bit across identical runs.
//
// The fix sorts the iteration order and breaks on an L1 delta below tolerance.
// Nothing asserted either property, so reverting it left the suite green.

// chainGraph builds a directed path n000 -> n001 -> ... whose last node is
// dangling. Its stationary distribution is strongly non-uniform, which is what
// makes convergence observable.
//
// A cycle would not work: on a regular graph the uniform starting vector is
// already the fixed point, so every budget agrees from the first pass and an
// implementation that never converges still looks converged.
func chainGraph(n int) map[string][]string {
	edges := make(map[string][]string, n)
	for i := 0; i < n-1; i++ {
		edges[fmt.Sprintf("n%03d", i)] = []string{fmt.Sprintf("n%03d", i+1)}
	}
	edges[fmt.Sprintf("n%03d", n-1)] = nil
	return edges
}

// onePass applies one exact PageRank iteration, including the uniform
// redistribution of dangling mass. Leaving the dangling term out measures the
// mass held by the chain's last node and calls it a convergence failure.
func onePass(edges map[string][]string, rank map[string]float64) map[string]float64 {
	const damping = 0.85
	n := float64(len(rank))
	next := make(map[string]float64, len(rank))
	base := (1 - damping) / n
	dangling := 0.0
	for node := range rank {
		next[node] = base
		if len(edges[node]) == 0 {
			dangling += rank[node]
		}
	}
	if dangling > 0 {
		share := damping * dangling / n
		for node := range next {
			next[node] += share
		}
	}
	for node, neighbours := range edges {
		if len(neighbours) == 0 {
			continue
		}
		share := damping * rank[node] / float64(len(neighbours))
		for _, m := range neighbours {
			next[m] += share
		}
	}
	return next
}

// TestPageRankDoesNotBurnABudgetItHasAlreadySatisfied is the convergence half.
// The returned vector is the same either way -- extra passes over a fixed
// point change nothing -- so the only thing that distinguishes breaking from
// not breaking is whether the passes are run at all. A budget three orders of
// magnitude past what this graph needs must therefore cost about nothing.
func TestPageRankDoesNotBurnABudgetItHasAlreadySatisfied(t *testing.T) {
	edges := chainGraph(40)

	start := time.Now()
	ranks := GlobalPageRank(edges, 2_000_000)
	elapsed := time.Since(start)

	if len(ranks) != 40 {
		t.Fatalf("got %d ranks, want 40", len(ranks))
	}
	// Two million passes over this graph take tens of seconds when they are
	// actually run; converging takes well under a hundred. The threshold is
	// deliberately loose -- it is separating "stopped" from "ran them all",
	// not measuring anything.
	if elapsed > 2*time.Second {
		t.Fatalf("a 2,000,000-pass budget took %s on a 40-node graph: the "+
			"iteration is running to the budget rather than stopping at the "+
			"fixed point", elapsed)
	}
}

// TestPageRankReturnsAConvergedVector pins what stopping early must not cost:
// the vector it stops on has to be a fixed point.
func TestPageRankReturnsAConvergedVector(t *testing.T) {
	edges := chainGraph(400)
	ranks := GlobalPageRank(edges, 100_000)

	residual := 0.0
	for node, want := range onePass(edges, ranks) {
		residual += math.Abs(want - ranks[node])
	}
	if residual > 1e-8 {
		t.Fatalf("L1 residual after one further pass is %g: the returned vector "+
			"is an unsettled prior, not the answer", residual)
	}

	// Total mass is conserved: dangling mass is redistributed rather than lost.
	total := 0.0
	for _, v := range ranks {
		total += v
	}
	if math.Abs(total-1) > 1e-9 {
		t.Fatalf("probability mass is %g, want 1: mass is leaking out of the walk", total)
	}
}

// TestPageRankIsDeterministicAcrossRuns pins the ordering half: float
// accumulation in Go map order made the stored vector differ bit-for-bit
// across identical runs, and the cortex is persisted from it.
func TestPageRankIsDeterministicAcrossRuns(t *testing.T) {
	edges := chainGraph(64)
	// Extra dangling nodes: their mass is summed in a separate loop, which is
	// the most order-sensitive part of the computation.
	for i := 0; i < 16; i++ {
		d := fmt.Sprintf("d%03d", i)
		n := fmt.Sprintf("n%03d", i)
		edges[d] = nil
		edges[n] = append(edges[n], d)
	}
	first := GlobalPageRank(edges, 200)
	for round := 0; round < 8; round++ {
		got := GlobalPageRank(edges, 200)
		for node, want := range first {
			if got[node] != want {
				t.Fatalf("round %d: %s = %v, first run gave %v: map-order float "+
					"accumulation makes the persisted vector differ across "+
					"identical runs", round, node, got[node], want)
			}
		}
	}
}
