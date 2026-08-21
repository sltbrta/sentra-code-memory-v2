package dense

import (
	"fmt"
	"testing"
)

// The index had no deletion at all, which is why erasure could not reach it:
// a deleted document stayed answerable by vector search after the corpus, the
// lexical index and the memory cortex had all dropped it.

func seededIndex(t *testing.T) *HNSW {
	t.Helper()
	index := NewHNSW(3, 16, 64)
	// Two documents with several chunks each, plus a decoy whose id shares a
	// prefix with one of them.
	for _, id := range []string{
		"doc-1#0", "doc-1#1", "doc-1#2",
		"doc-2#0", "doc-2#1",
		"doc-10#0", // must not be claimed by a "doc-1" purge
	} {
		vec := []float32{float32(len(id)), 1, 2}
		if err := index.UpsertWithMetadata(id, vec, HitMetadata{
			DocumentID: id[:len(id)-2], ChunkID: id,
		}); err != nil {
			t.Fatal(err)
		}
	}
	return index
}

func TestDeleteDocumentsRemovesEveryChunk(t *testing.T) {
	index := seededIndex(t)
	before := index.Len()

	removed := index.DeleteDocuments([]string{"doc-1"})
	if removed != 3 {
		t.Fatalf("removed %d vectors, want the document's 3 chunks", removed)
	}
	if index.Len() != before-3 {
		t.Fatalf("length went %d -> %d, want %d", before, index.Len(), before-3)
	}
	if residual := index.HasDocuments([]string{"doc-1"}); len(residual) != 0 {
		t.Fatalf("the document still has vectors: %v", residual)
	}
}

// TestDeleteDoesNotClaimAPrefixSibling is the mistake a bare string prefix
// makes: "doc-1" must not delete "doc-10".
func TestDeleteDoesNotClaimAPrefixSibling(t *testing.T) {
	index := seededIndex(t)
	index.DeleteDocuments([]string{"doc-1"})

	if residual := index.HasDocuments([]string{"doc-10"}); len(residual) != 1 {
		t.Fatal("purging doc-1 also removed doc-10: the match is a bare string " +
			"prefix rather than a chunk-id boundary")
	}
	if residual := index.HasDocuments([]string{"doc-2"}); len(residual) != 1 {
		t.Fatal("purging doc-1 removed doc-2")
	}
}

// TestSearchAfterDeleteIsCoherent is what compaction has to preserve. Every
// neighbour index in the graph refers to a slot that has moved, so a patched
// graph still points at live slots -- nothing crashes and the results are
// simply wrong.
func TestSearchAfterDeleteIsCoherent(t *testing.T) {
	index := seededIndex(t)
	index.DeleteDocuments([]string{"doc-1"})

	hits, _, err := index.SearchScopedMode([]float32{7, 1, 2}, 10, IndexIdentity{}, SearchModeExact)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) == 0 {
		t.Fatal("search returned nothing after a delete")
	}
	for _, hit := range hits {
		if vectorIDMatchesDocument(hit.VectorID, "doc-1") {
			t.Fatalf("a purged vector was returned by search: %s", hit.VectorID)
		}
		// Every returned id must still be a member of the index: a stale
		// neighbour index would surface an id that no longer exists.
		if _, ok := index.byID[hit.VectorID]; !ok {
			t.Fatalf("search returned %s, which is not in the index", hit.VectorID)
		}
	}

	// ANN mode walks the graph, so it is the mode that a bad rewire breaks.
	annHits, _, err := index.SearchScopedMode([]float32{7, 1, 2}, 10, IndexIdentity{}, SearchModeANN)
	if err != nil {
		t.Fatal(err)
	}
	for _, hit := range annHits {
		if _, ok := index.byID[hit.VectorID]; !ok {
			t.Fatalf("ANN search returned %s, which is not in the index", hit.VectorID)
		}
	}
}

// TestUpsertAfterDeleteStillReplaces keeps the id index consistent with the
// compacted slices: a stale byID entry would make a re-upsert either duplicate
// or overwrite the wrong slot.
func TestUpsertAfterDeleteStillReplaces(t *testing.T) {
	index := seededIndex(t)
	index.DeleteDocuments([]string{"doc-1"})
	after := index.Len()

	if err := index.Upsert("doc-2#0", []float32{9, 9, 9}); err != nil {
		t.Fatal(err)
	}
	if index.Len() != after {
		t.Fatalf("re-upserting an existing id grew the index from %d to %d: "+
			"the id map is stale after compaction", after, index.Len())
	}
	if err := index.Upsert("doc-1#0", []float32{1, 1, 1}); err != nil {
		t.Fatal(err)
	}
	if index.Len() != after+1 {
		t.Fatalf("re-adding a purged id did not grow the index: %d", index.Len())
	}
}

func TestDeleteOfAnUnknownDocumentIsANoOp(t *testing.T) {
	index := seededIndex(t)
	before := index.Len()
	if removed := index.DeleteDocuments([]string{"doc-absent"}); removed != 0 {
		t.Fatalf("removed %d for an absent document", removed)
	}
	if index.Len() != before {
		t.Fatalf("length changed from %d to %d", before, index.Len())
	}
}

// TestDeleteScalesToACompactedIndex keeps the compaction correct at a size
// where the graph wiring samples rather than compares everything.
func TestDeleteScalesToACompactedIndex(t *testing.T) {
	index := NewHNSW(3, 16, 64)
	for i := 0; i < 2000; i++ {
		id := fmt.Sprintf("doc-%04d#0", i)
		if err := index.Upsert(id, []float32{float32(i%97) + 1, float32(i%31) + 1, 1}); err != nil {
			t.Fatal(err)
		}
	}
	var purge []string
	for i := 0; i < 500; i++ {
		purge = append(purge, fmt.Sprintf("doc-%04d", i))
	}
	if removed := index.DeleteDocuments(purge); removed != 500 {
		t.Fatalf("removed %d, want 500", removed)
	}
	if index.Len() != 1500 {
		t.Fatalf("length %d, want 1500", index.Len())
	}
	if residual := index.HasDocuments(purge); len(residual) != 0 {
		t.Fatalf("%d purged documents still have vectors", len(residual))
	}
	hits, _, err := index.SearchScopedMode([]float32{50, 15, 1}, 10, IndexIdentity{}, SearchModeANN)
	if err != nil {
		t.Fatal(err)
	}
	for _, hit := range hits {
		if _, ok := index.byID[hit.VectorID]; !ok {
			t.Fatalf("search returned %s, which is not in the index", hit.VectorID)
		}
	}
}
