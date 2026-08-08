package ontology

import (
	"strings"
	"testing"
)

func TestExtractDocumentEdgesCites(t *testing.T) {
	t.Parallel()
	edges := ExtractDocumentEdges("gen-1", "doc-a", "See dsid_payments_root_cause for details and doc_other_report_v2.")
	if len(edges) == 0 {
		t.Fatal("expected cite edges")
	}
	found := map[string]bool{}
	for _, e := range edges {
		if e.DocumentSrc != "doc-a" {
			t.Fatalf("src = %q", e.DocumentSrc)
		}
		if e.Rel != RelCites {
			t.Fatalf("rel = %s", e.Rel)
		}
		if e.GenerationID != "gen-1" {
			t.Fatalf("gen = %s", e.GenerationID)
		}
		if e.Provenance != provenanceDet {
			t.Fatalf("provenance = %q", e.Provenance)
		}
		found[strings.ToLower(e.DocumentDst)] = true
	}
	if !found["dsid_payments_root_cause"] {
		t.Fatalf("missing dsid cite: %v", edges)
	}
}

func TestExtractDocumentEdgesSkipsSelf(t *testing.T) {
	t.Parallel()
	edges := ExtractDocumentEdges("gen-1", "dsid_self_document", "Self ref dsid_self_document only.")
	if len(edges) != 0 {
		t.Fatalf("expected no self-cites, got %v", edges)
	}
}

func TestBuildCoOccurrenceSharedTicket(t *testing.T) {
	t.Parallel()
	docs := map[string]string{
		"doc1": "Incident PROJ-1234 root cause in payments.",
		"doc2": "Follow-up for PROJ-1234 mitigation steps.",
		"doc3": "Unrelated gardening notes about flowers.",
	}
	g := BuildCoOccurrenceGraph("gen-1", docs, 50)
	if err := ValidateGraph(g); err != nil {
		t.Fatalf("validate: %v", err)
	}
	if g.GenerationID != "gen-1" {
		t.Fatalf("generation = %s", g.GenerationID)
	}
	if len(g.Edges) == 0 {
		t.Fatal("expected co-occurrence edges from shared ticket")
	}
	// Shared PROJ-1234 should link doc1 ↔ doc2 (RelCites for ticket refs).
	linked := false
	for _, e := range g.Edges {
		pair := e.DocumentSrc + "|" + e.DocumentDst
		if (pair == "doc1|doc2" || pair == "doc2|doc1") && e.Rel == RelCites {
			linked = true
			if e.Weight <= 0 {
				t.Fatalf("non-positive weight: %v", e)
			}
			if e.Provenance != provenanceDet {
				t.Fatalf("provenance = %q", e.Provenance)
			}
		}
	}
	if !linked {
		t.Fatalf("expected doc1–doc2 RelCites edge for PROJ-1234; edges=%v", g.Edges)
	}
	n := Neighbors(g, []string{"doc1"}, 10)
	foundDoc2 := false
	for _, id := range n {
		if id == "doc2" {
			foundDoc2 = true
		}
		if id == "doc3" {
			// doc3 may appear via other rare terms; not required to be absent.
		}
	}
	if !foundDoc2 {
		t.Fatalf("neighbors of doc1 missing doc2: %v (edges=%v)", n, g.Edges)
	}
}

func TestBuildCoOccurrenceMaxTermDocs(t *testing.T) {
	t.Parallel()
	// Term appearing in all docs should be skipped when maxTermDocs is tight.
	docs := map[string]string{
		"a": "SHARED-99 alpha",
		"b": "SHARED-99 beta",
		"c": "SHARED-99 gamma",
	}
	// maxTermDocs=2 excludes the ticket present in 3 docs.
	g := BuildCoOccurrenceGraph("gen-x", docs, 2)
	for _, e := range g.Edges {
		if e.Rel == RelCites && (e.DocumentSrc == "a" || e.DocumentDst == "a") {
			// SHARED-99 must not produce cites edges.
			t.Fatalf("expected SHARED-99 filtered by maxTermDocs; got %v", e)
		}
	}
}

func TestGenerationStorePutGetMerge(t *testing.T) {
	t.Parallel()
	s := NewGenerationStore()
	g := Graph{
		GenerationID: "gen-1",
		Edges: []Edge{
			{DocumentSrc: "a", DocumentDst: "b", Rel: RelCoProject, Weight: 1, GenerationID: "gen-1"},
		},
	}
	s.PutGraph(g)
	got, ok := s.GetGraph("gen-1")
	if !ok || len(got.Edges) != 1 {
		t.Fatalf("get = %+v ok=%v", got, ok)
	}
	// Mutating returned slice must not affect store.
	got.Edges[0].DocumentDst = "mutated"
	again, _ := s.GetGraph("gen-1")
	if again.Edges[0].DocumentDst != "b" {
		t.Fatal("store leaked mutation")
	}

	s.MergeEdges("gen-1", []Edge{
		{DocumentSrc: "b", DocumentDst: "c", Rel: RelCites, Weight: 0.5},
		// Higher weight wins on duplicate key.
		{DocumentSrc: "a", DocumentDst: "b", Rel: RelCoProject, Weight: 2},
	})
	merged, ok := s.GetGraph("gen-1")
	if !ok {
		t.Fatal("missing after merge")
	}
	if len(merged.Edges) != 2 {
		t.Fatalf("edges = %d want 2: %v", len(merged.Edges), merged.Edges)
	}
	var abWeight float64
	for _, e := range merged.Edges {
		if e.DocumentSrc == "a" && e.DocumentDst == "b" && e.Rel == RelCoProject {
			abWeight = e.Weight
		}
	}
	if abWeight != 2 {
		t.Fatalf("merge weight = %v want 2", abWeight)
	}
}

func TestGenerationStoreMergeCreates(t *testing.T) {
	t.Parallel()
	s := NewGenerationStore()
	s.MergeEdges("gen-new", []Edge{
		{DocumentSrc: "x", DocumentDst: "y", Rel: RelMentions, Weight: 1},
	})
	g, ok := s.GetGraph("gen-new")
	if !ok || len(g.Edges) != 1 {
		t.Fatalf("got %+v ok=%v", g, ok)
	}
	if g.GenerationID != "gen-new" {
		t.Fatalf("gen = %s", g.GenerationID)
	}
}
