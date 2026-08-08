package companydoc_test

import (
	"context"
	"strings"
	"testing"

	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/companydoc"
	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/gardener"
	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/ontology"
)

// Integration: JSONL import → gardener OnPublished → ontology graph → hop.
func TestProductPathIngestEnrichHop(t *testing.T) {
	t.Parallel()
	raw := `{"document_id":"d1","text":"Incident PROJ-99 MedThink RPO policy for active datasets."}
{"document_id":"d2","text":"PROJ-99 follow-up: MedThink mitigation steps and owners."}
{"document_id":"d3","text":"Unrelated picnic planning notes."}
`
	batch, err := companydoc.ImportJSONL(strings.NewReader(raw), "company-src", "gen-1")
	if err != nil {
		t.Fatal(err)
	}
	docs := companydoc.TextMap(batch.Documents)
	store := ontology.NewGenerationStore()
	hopper := ontology.StoreHopper{Store: store}
	q := &gardener.MemoryQueue{}
	enricher := &gardener.GenerationEnricher{
		Queue:     q,
		Budget:    gardener.Budget{MaxJobs: 100, MaxConcurrent: 8},
		GraphSink: hopper,
	}
	n, err := enricher.OnPublished(context.Background(), "gen-1", docs)
	if err != nil {
		t.Fatal(err)
	}
	if n < 6 {
		t.Fatalf("enqueued %d, want >= 6", n)
	}
	g, ok := store.GetGraph("gen-1")
	if !ok || len(g.Edges) == 0 {
		t.Fatalf("graph missing or empty: ok=%v edges=%d", ok, len(g.Edges))
	}
	neighbors := hopper.Expand("gen-1", []string{"d1"}, 8)
	found := false
	for _, id := range neighbors {
		if id == "d2" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected d2 neighbor of d1 via PROJ-99/MedThink, got %v edges=%d", neighbors, len(g.Edges))
	}
	// Run one gardener wave (deterministic workers)
	sched := &gardener.Scheduler{
		Queue:   q,
		Workers: gardener.DefaultWorkers(),
		Budget:  gardener.DefaultBudget(),
	}
	recs, err := sched.RunWave(context.Background(), "w1")
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) == 0 {
		t.Fatal("expected worker receipts")
	}
	okCount := 0
	for _, r := range recs {
		if r.OK {
			okCount++
		}
	}
	if okCount == 0 {
		t.Fatalf("no ok receipts: %+v", recs)
	}
}
