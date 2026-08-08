package hosted

import (
	"context"
	"testing"
)

func TestEnrichAfterIngestWarmsSidecars(t *testing.T) {
	c := OpenMemory("enrich-test")
	ctx := context.Background()
	if err := c.EnsureSchema(ctx); err != nil {
		t.Fatal(err)
	}
	docs := []LocalDocument{
		{ID: "d1", Text: "RPO is 4 hours for payment DB. Owner is SRE."},
		{ID: "d2", Text: "Deploy procedure uses canary and rollback steps."},
	}
	res, err := c.BurstIngestLocal(ctx, docs, 2)
	if err != nil {
		t.Fatal(err)
	}
	if res.Ingested < 1 && res.Upserted < 1 {
		t.Fatalf("ingest empty: %+v", res)
	}
	// Enrich runs inside BurstIngestLocal by default.
	if res.EnrichJobs < 1 && res.EnrichSidecars < 1 {
		// Try explicit enrich.
		er, eerr := c.EnrichAfterIngest(ctx, "enrich-test", res.GenerationID, map[string]string{
			"d1": docs[0].Text, "d2": docs[1].Text,
		})
		if eerr != nil {
			t.Fatal(eerr)
		}
		if er.JobsEnqueued < 1 && er.SidecarsWarm < 1 {
			t.Fatalf("enrich empty: %+v", er)
		}
	}
}

func TestQualityDoc2QueryAndPhraseHop(t *testing.T) {
	qs := qualityDoc2QueryVariants("What is the RPO and RTO for MedThink?")
	if len(qs) == 0 {
		t.Fatal("expected d2q variants")
	}
	ph := phraseHopQueries("RPO payment", []Passage{{Text: "MedThink RPO is 15m", DocumentID: "a"}})
	if len(ph) == 0 {
		t.Fatal("expected phrase hop queries")
	}
}
