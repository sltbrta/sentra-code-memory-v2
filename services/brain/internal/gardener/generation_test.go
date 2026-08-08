package gardener

import (
	"context"
	"testing"

	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/ontology"
)

func TestOnPublishedEnqueuesPlannedJobs(t *testing.T) {
	t.Parallel()
	q := &MemoryQueue{}
	e := &GenerationEnricher{
		Queue:  q,
		Budget: Budget{MaxJobs: 100},
	}
	docs := map[string]string{
		"d1": "MedThink RPO is 15 minutes. See OPS-42.",
		"d2": "OpenFGA model for ACL.",
	}
	n, err := e.OnPublished(context.Background(), "gen-pub", docs)
	if err != nil {
		t.Fatal(err)
	}
	// 2 docs × 3 default kinds
	if n != 6 {
		t.Fatalf("enqueued = %d, want 6", n)
	}
	claimed, err := q.Claim(context.Background(), "w", 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(claimed) != 6 {
		t.Fatalf("claimed = %d", len(claimed))
	}
	byKind := map[JobKind]int{}
	for _, j := range claimed {
		byKind[j.Kind]++
		if j.GenerationID != "gen-pub" {
			t.Fatalf("gen = %q", j.GenerationID)
		}
		if j.Payload["text"] == "" {
			t.Fatalf("empty text on %s", j.ID)
		}
	}
	if byKind[JobDoc2Query] != 2 || byKind[JobEdgePropose] != 2 || byKind[JobContextHeader] != 2 {
		t.Fatalf("byKind = %+v", byKind)
	}
}

func TestOnPublishedRespectsMaxJobs(t *testing.T) {
	t.Parallel()
	q := &MemoryQueue{}
	e := &GenerationEnricher{
		Queue:  q,
		Budget: Budget{MaxJobs: 2},
	}
	docs := map[string]string{"a": "1", "b": "2", "c": "3"}
	n, err := e.OnPublished(context.Background(), "g", docs)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("enqueued = %d, want 2", n)
	}
}

func TestOnPublishedNilQueue(t *testing.T) {
	t.Parallel()
	e := &GenerationEnricher{}
	_, err := e.OnPublished(context.Background(), "g", map[string]string{"d": "t"})
	if err == nil {
		t.Fatal("expected error for nil queue")
	}
}

func TestOnPublishedEmptyDocs(t *testing.T) {
	t.Parallel()
	q := &MemoryQueue{}
	e := &GenerationEnricher{Queue: q}
	n, err := e.OnPublished(context.Background(), "g", nil)
	if err != nil || n != 0 {
		t.Fatalf("n=%d err=%v", n, err)
	}
}

func TestOnPublishedBuildsOntologyGraph(t *testing.T) {
	t.Parallel()
	q := &MemoryQueue{}
	store := ontology.NewGenerationStore()
	hopper := ontology.StoreHopper{Store: store}
	e := &GenerationEnricher{
		Queue:     q,
		Budget:    Budget{MaxJobs: 100},
		GraphSink: hopper,
	}
	docs := map[string]string{
		"d1": "Incident PROJ-99 root cause for MedThink RPO.",
		"d2": "Follow-up PROJ-99 mitigation on MedThink.",
	}
	if _, err := e.OnPublished(context.Background(), "gen-g", docs); err != nil {
		t.Fatal(err)
	}
	g, ok := store.GetGraph("gen-g")
	if !ok {
		t.Fatal("expected graph for gen-g")
	}
	if len(g.Edges) == 0 {
		t.Fatal("expected co-occurrence edges")
	}
	// Hopper expand should surface neighbors of d1
	n := hopper.Expand("gen-g", []string{"d1"}, 8)
	if len(n) == 0 {
		t.Fatalf("expected neighbors, edges=%d", len(g.Edges))
	}
}
