package dense

import (
	"fmt"
	"math"
	"testing"
)

// Upsert scanned the id slice linearly to find an existing entry, so loading n
// vectors cost O(n^2) comparisons: the load path calls Upsert once per vector,
// and each one walked everything inserted before it.
//
// The fix is an id index. The growth rate can only be observed with a clock,
// so it lives in a benchmark; what is asserted here is the behaviour the
// linear scan provided and the map must preserve.

func loadVectors(b *HNSW, n int) {
	for i := 0; i < n; i++ {
		vec := []float32{float32(i%97) + 1, float32(i%31) + 1, float32(i%7) + 1}
		_ = b.Upsert(fmt.Sprintf("vec-%06d", i), vec)
	}
}

// BenchmarkUpsertLoad measures the load cost, and is a benchmark rather than a
// test on purpose.
//
// The property is a growth rate, and the only way to observe it here is with a
// clock. Measured on an M4 Pro, loading 40,000 vectors: 1.831s with the linear
// scan, 0.809s with the map -- and the doubling ratio from 20,000 to 40,000
// moves from 2.81 to 2.01. Those separate cleanly on an idle machine and do
// not separate at all inside a parallel `go test -race ./...` run, where this
// package shares its cores with every other one. This repository already
// carries an open hardening entry for a wall-clock assertion that failed
// exactly that way, so adding another would be repeating a known mistake.
//
// What is asserted deterministically is below: the replacement semantics the
// linear scan existed to provide, which the map has to preserve.
//
// Run with: go test ./brain/internal/dense/ -run '^$' -bench UpsertLoad
func BenchmarkUpsertLoad(b *testing.B) {
	for _, n := range []int{10_000, 20_000, 40_000} {
		b.Run(fmt.Sprintf("vectors_%d", n), func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				index := NewHNSW(3, 16, 64)
				loadVectors(index, n)
			}
		})
	}
}

// TestUpsertReplacesRatherThanDuplicates is the behaviour the linear scan was
// there for, and the property the map has to preserve.
func TestUpsertReplacesRatherThanDuplicates(t *testing.T) {
	index := NewHNSW(3, 16, 64)
	if err := index.Upsert("a", []float32{1, 0, 0}); err != nil {
		t.Fatal(err)
	}
	if err := index.Upsert("b", []float32{0, 1, 0}); err != nil {
		t.Fatal(err)
	}
	if got := index.Len(); got != 2 {
		t.Fatalf("len = %d, want 2", got)
	}

	// Replace "a" with a vector pointing the other way.
	if err := index.Upsert("a", []float32{0, 0, 1}); err != nil {
		t.Fatal(err)
	}
	if got := index.Len(); got != 2 {
		t.Fatalf("a replacement grew the index to %d", got)
	}

	hits, _, err := index.SearchScopedMode([]float32{0, 0, 1}, 1, IndexIdentity{}, SearchModeExact)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].VectorID != "a" {
		t.Fatalf("replacement was not applied: %+v", hits)
	}
	if math.Abs(float64(hits[0].Score)-1) > 1e-6 {
		t.Fatalf("replacement vector is not the one stored: score %v", hits[0].Score)
	}
}

// TestClonePreservesReplacementSemantics covers the second place the id index
// is built. A clone whose map was not rebuilt would start duplicating ids.
func TestClonePreservesReplacementSemantics(t *testing.T) {
	index := NewHNSW(3, 16, 64)
	for i := 0; i < 8; i++ {
		if err := index.Upsert(fmt.Sprintf("v-%d", i), []float32{float32(i + 1), 1, 1}); err != nil {
			t.Fatal(err)
		}
	}
	clone := index.Clone()
	before := clone.Len()
	if err := clone.Upsert("v-3", []float32{9, 9, 9}); err != nil {
		t.Fatal(err)
	}
	if clone.Len() != before {
		t.Fatalf("upserting an existing id into a clone grew it from %d to %d: "+
			"the clone's id index was not rebuilt", before, clone.Len())
	}
}
