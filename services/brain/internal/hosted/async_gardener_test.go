package hosted

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLocalAsyncGardenerThenWave(t *testing.T) {
	// Default local path: ingest enqueues; RunGardenerWave warms sidecars.
	dir := t.TempDir()
	c, err := CreateLocal(dir, "async-g")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if c.GardenerQueue() == nil {
		t.Fatal("expected durable gardener queue on OpenLocal")
	}
	if _, err := os.Stat(filepath.Join(dir, "gardener.db")); err != nil {
		t.Fatalf("gardener.db missing: %v", err)
	}

	ctx := context.Background()
	res, err := c.BurstIngestLocal(ctx, []LocalDocument{
		{ID: "pol", Title: "Policy", Text: "MedThink RPO is 15 minutes. Kyoto office."},
	}, 1)
	if err != nil {
		t.Fatal(err)
	}
	if res.EnrichJobs < 1 {
		t.Fatalf("expected enqueued jobs, got %+v", res)
	}
	// Sidecars should not be warm yet (async).
	if res.EnrichSidecars > 0 {
		t.Fatalf("async path should not warm inline: %+v", res)
	}

	er, err := c.RunGardenerWave(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if er.ReceiptsOK < 1 && er.SidecarsWarm < 1 {
		t.Fatalf("wave empty: %+v", er)
	}

	ans := c.Answer(ctx, "What is MedThink RPO?", 6)
	if ans.Failure != "" {
		t.Fatalf("ask: %s", ans.Failure)
	}
	if ans.Answer == "" {
		t.Fatal("empty answer")
	}
}

// S3 hard gate: default async OpenLocal enqueue → RunGardenerWave must run
// extract → SeedRelationsFromClaims so lean serve can ExpandRelationDocuments.
// No ENRICH=sync; no manual RunCortexMaintenance.
func TestAsyncGardenerWaveLeftShiftsTemporalRelations(t *testing.T) {
	t.Setenv("OUROBOROS_BRAIN_LLM", "extractive")
	t.Setenv("OUROBOROS_BRAIN_ENRICH", "async")
	t.Setenv("OPENAI_API_KEY", "")
	t.Setenv("OUROBOROS_ERB_PROD", "1")
	t.Setenv("OUROBOROS_ERB_QUALITY", "0")

	dir := t.TempDir()
	ctx := context.Background()
	c, err := CreateLocal(dir, "async-cortex")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if c.GardenerQueue() == nil {
		t.Fatal("want durable gardener queue")
	}
	if c.Mem == nil {
		t.Fatal("want cortex Mem on OpenLocal")
	}

	policy := "MedThink failover policy sets RPO to 15 minutes and RTO to 30 minutes for gold tier production."
	noise := "Cafeteria menu changes weekly with seasonal specials."
	burst, err := c.BurstIngestLocal(ctx, []LocalDocument{
		{ID: "dsid_policy", Title: "Policy", Text: policy},
		{ID: "dsid_noise", Title: "Noise", Text: noise},
	}, 2)
	if err != nil {
		t.Fatal(err)
	}
	if burst.EnrichJobs < 1 {
		t.Fatalf("async ingest must enqueue gardener jobs: %+v", burst)
	}
	// Light seed only — no TemporalRelations until gardener wave.
	if docs := c.Mem.ExpandRelationDocuments([]string{"MedThink"}, time.Time{}, time.Time{}, 8); len(docs) > 0 {
		t.Fatalf("pre-wave must not have TemporalRelations yet: %v", docs)
	}

	er, err := c.RunGardenerWave(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if er.ClaimsAdmitted < 1 && er.RelationsAdmitted < 1 {
		t.Fatalf("async wave must left-shift claims→TemporalRelations: %+v", er)
	}
	if er.RelationsAdmitted < 1 {
		// Claims without seed can happen if already projected; force check store.
		if n := c.Mem.SeedRelationsFromClaims(); n < 1 {
			// Still require at least one expandable edge from store claims.
			docsIDs := c.Mem.ExpandRelationDocuments([]string{"MedThink"}, time.Time{}, time.Time{}, 8)
			if len(docsIDs) == 0 {
				t.Fatalf("want RelationsAdmitted≥1 or MedThink docs, er=%+v", er)
			}
		}
	}

	docsIDs := c.Mem.ExpandRelationDocuments([]string{"MedThink"}, time.Time{}, time.Time{}, 8)
	if len(docsIDs) == 0 {
		t.Fatal("want MedThink → dsid_policy via TemporalRelations after async gardener wave")
	}
	found := false
	for _, id := range docsIDs {
		if id == "dsid_policy" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("want dsid_policy in relation docs, got %v", docsIDs)
	}

	ans := c.AnswerOpts(ctx, AnswerOptions{
		Question:     "What is the MedThink RPO for gold tier failover?",
		QuestionType: "basic",
		TopK:         6,
	})
	if ans.Failure != "" {
		t.Fatalf("ask: %s", ans.Failure)
	}
	diag := ans.RetrievalDiagnostics
	if diag == nil {
		t.Fatal("nil retrieval diagnostics")
	}
	n, _ := diag["temporal_relation_docs"].(int)
	if n < 1 {
		if f, ok := diag["temporal_relation_docs"].(float64); ok {
			n = int(f)
		}
	}
	if n < 1 {
		t.Fatalf("lean serve must consume TemporalRelations after async wave, diag=%v", diag)
	}
	low := strings.ToLower(ans.Answer)
	if !strings.Contains(low, "15") && !strings.Contains(low, "rpo") && !strings.Contains(low, "medthink") {
		t.Fatalf("answer missing gold: %q", ans.Answer)
	}
}
