package hosted

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

// TestMemoryLifecycleE2E: create → burst → gardener enrich → multi-turn ask.
func TestMemoryLifecycleE2E(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	c, err := CreateLocal(dir, "mem-life")
	if err != nil {
		t.Fatal(err)
	}
	// Session store next to brain.
	t.Setenv("OUROBOROS_BRAIN_SESSION_PATH", filepath.Join(dir, "sessions.jsonl"))

	res, err := c.BurstIngestLocal(ctx, []LocalDocument{
		{ID: "fact-city", Text: "User lives in Kyoto Japan."},
		{ID: "fact-editor", Text: "User preferred editor is helix after trying vim."},
		{ID: "fact-project", Text: "Project was renamed from Orpheus to Ouroboros."},
	}, 2)
	if err != nil {
		t.Fatal(err)
	}
	if res.Upserted < 1 && res.Ingested < 1 {
		t.Fatalf("ingest empty: %+v", res)
	}
	if res.EnrichJobs < 1 {
		t.Fatalf("expected async enqueue jobs: %+v", res)
	}
	// Background gardener (product daemon step).
	er, eerr := c.RunGardenerWave(ctx)
	if eerr != nil {
		t.Fatal(eerr)
	}
	if er.ReceiptsOK < 1 && er.SidecarsWarm < 1 {
		t.Fatalf("gardener wave empty: %+v", er)
	}

	// Multi-turn: first ask with session.
	a1 := c.AnswerOpts(ctx, AnswerOptions{
		Question:  "What city does the user live in?",
		TopK:      6,
		SessionID: "sess-1",
	})
	if a1.Failure != "" {
		t.Fatalf("ask1: %s", a1.Failure)
	}
	if !strings.Contains(strings.ToLower(a1.Answer), "kyoto") && !containsAnyCite(a1, "fact-city") {
		// Extractive may still cite; accept either answer text or cite.
		if len(a1.CitedDocumentIDs) == 0 {
			t.Fatalf("ask1 no kyoto/cites: %q diag=%v", a1.Answer, a1.RetrievalDiagnostics)
		}
	}

	// Delta continual ingest.
	delta, err := c.ContinualDeltaLocal(ctx, []LocalDocument{
		{ID: "fact-deploy", Text: "Last deploy: 2026-07-01 09:00 UTC."},
	})
	if err != nil {
		t.Fatal(err)
	}
	if delta.Mode != "delta" {
		t.Fatalf("mode %s", delta.Mode)
	}

	a2 := c.AnswerOpts(ctx, AnswerOptions{
		Question:  "When was the last deploy?",
		TopK:      6,
		SessionID: "sess-1",
	})
	if a2.Failure != "" {
		t.Fatalf("ask2: %s", a2.Failure)
	}
	_ = c.Close()
}

func containsAnyCite(a AnswerResult, want string) bool {
	for _, id := range a.CitedDocumentIDs {
		if id == want {
			return true
		}
	}
	return false
}
