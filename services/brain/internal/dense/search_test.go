package dense_test

import (
	"math"
	"testing"

	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/dense"
)

func TestMemoryStoreNearestNeighborOrthogonal(t *testing.T) {
	store := dense.NewMemoryStore()
	// Three orthogonal axes in R3.
	if err := store.Upsert("x", []float32{1, 0, 0}); err != nil {
		t.Fatal(err)
	}
	if err := store.Upsert("y", []float32{0, 1, 0}); err != nil {
		t.Fatal(err)
	}
	if err := store.Upsert("z", []float32{0, 0, 1}); err != nil {
		t.Fatal(err)
	}

	// Query aligned with y should recover y as nearest, score ~1.
	hits := store.Search([]float32{0, 1, 0}, 1)
	if len(hits) != 1 {
		t.Fatalf("len(hits)=%d want 1", len(hits))
	}
	if hits[0].DocumentID != "y" {
		t.Fatalf("nearest=%q want y", hits[0].DocumentID)
	}
	if math.Abs(hits[0].Score-1.0) > 1e-6 {
		t.Fatalf("score=%v want ~1", hits[0].Score)
	}

	// Query mostly x with a little z: top-2 should be x then z.
	hits = store.Search([]float32{0.9, 0, 0.1}, 2)
	if len(hits) != 2 {
		t.Fatalf("len(hits)=%d want 2", len(hits))
	}
	if hits[0].DocumentID != "x" {
		t.Fatalf("top0=%q want x", hits[0].DocumentID)
	}
	if hits[1].DocumentID != "z" {
		t.Fatalf("top1=%q want z", hits[1].DocumentID)
	}
	if hits[0].Score <= hits[1].Score {
		t.Fatalf("scores not descending: %v <= %v", hits[0].Score, hits[1].Score)
	}

	// Orthogonal vectors: cosine of x vs y is 0.
	if c := dense.Cosine([]float32{1, 0, 0}, []float32{0, 1, 0}); math.Abs(c) > 1e-6 {
		t.Fatalf("cosine orthogonal=%v want 0", c)
	}
}

func TestMemoryStoreUpsertDimMismatch(t *testing.T) {
	store := dense.NewMemoryStore()
	if err := store.Upsert("a", []float32{1, 0}); err != nil {
		t.Fatal(err)
	}
	if err := store.Upsert("b", []float32{1, 0, 0}); err == nil {
		t.Fatal("expected dim mismatch error")
	}
}

func TestMemoryStoreUpsertReplace(t *testing.T) {
	store := dense.NewMemoryStore()
	if err := store.Upsert("a", []float32{1, 0}); err != nil {
		t.Fatal(err)
	}
	if err := store.Upsert("a", []float32{0, 1}); err != nil {
		t.Fatal(err)
	}
	hits := store.Search([]float32{0, 1}, 1)
	if len(hits) != 1 || hits[0].DocumentID != "a" {
		t.Fatalf("hits=%v", hits)
	}
	if math.Abs(hits[0].Score-1.0) > 1e-6 {
		t.Fatalf("score=%v", hits[0].Score)
	}
}

func TestMemoryStoreSearchEmptyAndMismatch(t *testing.T) {
	store := dense.NewMemoryStore()
	if hits := store.Search([]float32{1}, 1); hits != nil {
		t.Fatalf("empty store: got %v", hits)
	}
	if err := store.Upsert("a", []float32{1, 0}); err != nil {
		t.Fatal(err)
	}
	if hits := store.Search([]float32{1}, 1); hits != nil {
		t.Fatalf("dim mismatch query: got %v", hits)
	}
	if hits := store.Search(nil, 1); hits != nil {
		t.Fatalf("nil query: got %v", hits)
	}
}

func TestMemoryStoreDefensiveCopy(t *testing.T) {
	store := dense.NewMemoryStore()
	vec := []float32{1, 0, 0}
	if err := store.Upsert("a", vec); err != nil {
		t.Fatal(err)
	}
	vec[0] = 0
	got, ok := store.Get("a")
	if !ok || got[0] != 1 {
		t.Fatalf("store mutated by caller slice: got %v", got)
	}
	got[0] = 0
	got2, _ := store.Get("a")
	if got2[0] != 1 {
		t.Fatalf("Get return mutated store: %v", got2)
	}
}
