package gardener

import (
	"context"
	"testing"
)

func TestDeterministicEdgeWorkerCites(t *testing.T) {
	t.Parallel()
	w := DeterministicEdgeWorker{}
	if w.Kind() != JobEdgePropose {
		t.Fatalf("kind = %s", w.Kind())
	}
	rec, err := w.Run(context.Background(), Job{
		ID: "j1", Kind: JobEdgePropose, GenerationID: "g1", DocumentID: "doc-self",
		Payload: map[string]string{
			"text": "See also dsid_abcdef12345678 for the policy and doc_otherdoc999999 notes.",
		},
	}, Budget{})
	if err != nil {
		t.Fatal(err)
	}
	if !rec.OK {
		t.Fatalf("not ok: %+v", rec)
	}
	if rec.Artifacts < 1 {
		t.Fatalf("expected cite edges, got %d", rec.Artifacts)
	}
}

func TestDeterministicEdgeWorkerEmptyText(t *testing.T) {
	t.Parallel()
	w := DeterministicEdgeWorker{}
	rec, err := w.Run(context.Background(), Job{
		ID: "j2", Kind: JobEdgePropose, GenerationID: "g1",
		Payload: map[string]string{"text": "   "},
	}, Budget{})
	if err != nil {
		t.Fatal(err)
	}
	if rec.OK || rec.Error != "empty_text" {
		t.Fatalf("want empty_text fail, got %+v", rec)
	}
}

func TestDeterministicEdgeWorkerNoCitesOK(t *testing.T) {
	t.Parallel()
	w := DeterministicEdgeWorker{}
	rec, err := w.Run(context.Background(), Job{
		ID: "j3", Kind: JobEdgePropose, GenerationID: "g1", DocumentID: "d1",
		Payload: map[string]string{"text": "Tracked in PROJ-99 only."},
	}, Budget{})
	if err != nil {
		t.Fatal(err)
	}
	// Tickets alone are co-occurrence graph signals, not single-doc cite edges.
	if !rec.OK {
		t.Fatalf("want ok: %+v", rec)
	}
	if rec.Artifacts != 0 {
		t.Fatalf("want 0 cite edges for ticket-only text, got %d", rec.Artifacts)
	}
}
