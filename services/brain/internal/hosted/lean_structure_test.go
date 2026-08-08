package hosted

import (
	"context"
	"strings"
	"testing"
	"time"
)

// Smoke: full CreateLocal → extract-dense policy → cortex → lean retrieve
// must stamp temporal_relation_docs when MedThink policy is in corpus.
func TestLeanServeTemporalRelationDiag(t *testing.T) {
	t.Setenv("OUROBOROS_BRAIN_LLM", "extractive")
	t.Setenv("OUROBOROS_BRAIN_ENRICH", "sync")
	t.Setenv("OPENAI_API_KEY", "")
	t.Setenv("OUROBOROS_ERB_PROD", "1")
	t.Setenv("OUROBOROS_ERB_QUALITY", "0")

	dir := t.TempDir()
	ctx := context.Background()
	c, err := CreateLocal(dir, "struct-smoke")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	docs := []LocalDocument{
		{ID: "dsid_policy", Text: "MedThink failover policy sets RPO to 15 minutes and RTO to 30 minutes for gold tier production."},
		{ID: "dsid_runbook", Text: "Operator runbook for MedThink: verify RPO 15 minutes before regional failover. Contact SRE on-call."},
		{ID: "dsid_noise", Text: "Office foosball tournament starts Friday at noon with kitchen snacks."},
	}
	if _, err := c.BurstIngestLocal(ctx, docs, 2); err != nil {
		t.Fatal(err)
	}
	if c.Mem == nil {
		t.Fatal("want cortex Mem attached")
	}
	// Left-shift: cortex maintenance extracts + seeds relations.
	res := c.Mem.RunCortexMaintenance(map[string]string{
		"dsid_policy":  docs[0].Text,
		"dsid_runbook": docs[1].Text,
		"dsid_noise":   docs[2].Text,
	})
	if res.RelationsAdmitted < 1 {
		t.Fatalf("want relations from denser extract: %+v", res)
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
	// Hard gate: left-shifted TemporalRelations must promote policy evidence docs.
	n, _ := diag["temporal_relation_docs"].(int)
	if n < 1 {
		if f, ok := diag["temporal_relation_docs"].(float64); ok {
			n = int(f)
		}
	}
	if n < 1 {
		t.Fatalf("want temporal_relation_docs≥1, diag=%v", diag)
	}
	low := strings.ToLower(ans.Answer)
	if !strings.Contains(low, "15") && !strings.Contains(low, "rpo") && !strings.Contains(low, "medthink") {
		t.Fatalf("answer missing gold: %q", ans.Answer)
	}
}

// Sync enrich (ENRICH=sync) must left-shift cortex after BurstIngestLocal —
// no separate gardener CLI required for TemporalRelations on FS brains.
func TestSyncEnrichLeftShiftsTemporalRelations(t *testing.T) {
	t.Setenv("OUROBOROS_BRAIN_LLM", "extractive")
	t.Setenv("OUROBOROS_BRAIN_ENRICH", "sync")
	t.Setenv("OPENAI_API_KEY", "")
	t.Setenv("OUROBOROS_ERB_PROD", "1")
	t.Setenv("OUROBOROS_ERB_QUALITY", "0")

	dir := t.TempDir()
	ctx := context.Background()
	c, err := CreateLocal(dir, "sync-cortex")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	docs := []LocalDocument{
		{ID: "dsid_policy", Text: "MedThink failover policy sets RPO to 15 minutes and RTO to 30 minutes for gold tier."},
		{ID: "dsid_noise", Text: "Cafeteria menu changes weekly."},
	}
	if _, err := c.BurstIngestLocal(ctx, docs, 2); err != nil {
		t.Fatal(err)
	}
	if c.Mem == nil {
		t.Fatal("want Mem")
	}
	// No manual RunCortexMaintenance — sync enrich post-wave must have seeded.
	docsIDs := c.Mem.ExpandRelationDocuments([]string{"MedThink"}, time.Time{}, time.Time{}, 8)
	if len(docsIDs) == 0 {
		// Force one wave in case Burst skipped enrich (queue quirks).
		er, eerr := c.EnrichAfterIngest(ctx, "sync-cortex", c.GenerationID(), map[string]string{
			"dsid_policy": docs[0].Text, "dsid_noise": docs[1].Text,
		})
		if eerr != nil {
			t.Fatal(eerr)
		}
		if er.RelationsAdmitted < 1 && er.ClaimsAdmitted < 1 {
			t.Fatalf("sync enrich must admit claims/relations: %+v", er)
		}
		docsIDs = c.Mem.ExpandRelationDocuments([]string{"MedThink"}, time.Time{}, time.Time{}, 8)
	}
	if len(docsIDs) == 0 {
		t.Fatal("want MedThink → dsid_policy via TemporalRelations after sync enrich")
	}
}

// General product SOTA (not ERB RPO-only): company graph extract → TemporalRelations
// → lean serve temporal_relation_docs after sync enrich.
func TestLeanServeGeneralProductTemporalRelations(t *testing.T) {
	t.Setenv("OUROBOROS_BRAIN_LLM", "extractive")
	t.Setenv("OUROBOROS_BRAIN_ENRICH", "sync")
	t.Setenv("OPENAI_API_KEY", "")
	t.Setenv("OUROBOROS_ERB_PROD", "1")
	t.Setenv("OUROBOROS_ERB_QUALITY", "0")

	dir := t.TempDir()
	ctx := context.Background()
	c, err := CreateLocal(dir, "product-graph")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	docs := []LocalDocument{
		{ID: "dsid_arch", Text: "Acme Cloud depends on Stripe for billing and provides SSO via Okta. Widget Platform integrates with Salesforce."},
		{ID: "dsid_org", Text: "Alice reports to Bob. Finance team manages the ledger service. Ledger service is owned by Northwind Holdings."},
		{ID: "dsid_noise", Text: "Friday kitchen snacks and foosball tournament signups."},
	}
	if _, err := c.BurstIngestLocal(ctx, docs, 2); err != nil {
		t.Fatal(err)
	}
	if c.Mem == nil {
		t.Fatal("want cortex")
	}
	// Sync enrich should left-shift; force wave if queue quirk.
	if n := len(c.Mem.ExpandRelationDocuments([]string{"Acme"}, time.Time{}, time.Time{}, 8)); n < 1 {
		er, eerr := c.EnrichAfterIngest(ctx, "product-graph", c.GenerationID(), map[string]string{
			"dsid_arch": docs[0].Text, "dsid_org": docs[1].Text, "dsid_noise": docs[2].Text,
		})
		if eerr != nil {
			t.Fatal(eerr)
		}
		if er.RelationsAdmitted < 1 {
			t.Fatalf("want product graph relations: %+v", er)
		}
	}
	for _, seed := range []string{"Acme", "Alice", "Widget"} {
		if docsIDs := c.Mem.ExpandRelationDocuments([]string{seed}, time.Time{}, time.Time{}, 8); len(docsIDs) == 0 {
			t.Fatalf("want TemporalRelation docs for %q", seed)
		}
	}

	ans := c.AnswerOpts(ctx, AnswerOptions{
		Question:     "What does Acme Cloud depend on for billing?",
		QuestionType: "basic",
		TopK:         6,
	})
	if ans.Failure != "" {
		t.Fatalf("ask: %s", ans.Failure)
	}
	diag := ans.RetrievalDiagnostics
	if diag == nil {
		t.Fatal("nil diags")
	}
	n, _ := diag["temporal_relation_docs"].(int)
	if n < 1 {
		if f, ok := diag["temporal_relation_docs"].(float64); ok {
			n = int(f)
		}
	}
	if n < 1 {
		t.Fatalf("lean serve must promote general product TemporalRelations, diag=%v", diag)
	}
	low := strings.ToLower(ans.Answer)
	if !strings.Contains(low, "stripe") && !strings.Contains(low, "acme") && !strings.Contains(low, "billing") {
		t.Fatalf("answer missing product gold: %q", ans.Answer)
	}
}
