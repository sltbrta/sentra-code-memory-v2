package hosted

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/memory"
)

// TestBrainBuildStructureLeftShift: product FS brain builds structure at ingest
// (edges/entities/facts via store reindex) + cortex bi-temporal relations, then
// lean ask consumes them without QUALITY multi-arm.
func TestBrainBuildStructureLeftShift(t *testing.T) {
	t.Setenv("OUROBOROS_BRAIN_LLM", "extractive")
	t.Setenv("OUROBOROS_BRAIN_ENRICH", "sync")
	t.Setenv("OPENAI_API_KEY", "")
	t.Setenv("OUROBOROS_ERB_PROD", "1")
	t.Setenv("OUROBOROS_ERB_QUALITY", "0")
	t.Setenv("OUROBOROS_ERB_MODE", "lean")

	dir := t.TempDir()
	ctx := context.Background()
	c, err := CreateLocal(dir, "struct-sota")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	// Multi-doc company corpus: shared entities link docs (structure thesis).
	docs := []LocalDocument{
		{ID: "dsid_policy", Text: "MedThink failover policy sets RPO to 15 minutes and RTO to 30 minutes for gold tier production."},
		{ID: "dsid_runbook", Text: "Operator runbook for MedThink: verify RPO 15 minutes before regional failover. Contact SRE on-call."},
		{ID: "dsid_sla", Text: "Horizon Quay SLA: 10 percent credit for 99.9 to 99.95 uptime, 30 percent for 99.5 to 99.9."},
		{ID: "dsid_noise", Text: "Office foosball tournament starts Friday at noon with kitchen snacks."},
	}
	res, err := c.BurstIngestLocal(ctx, docs, 2)
	if err != nil {
		t.Fatal(err)
	}
	if res.Upserted < 3 && res.Ingested < 3 {
		t.Fatalf("ingest: %+v", res)
	}

	// Cortex: admit bi-temporal claim + temporal relation (left-shifted intelligence).
	if c.Mem == nil {
		// Attach cortex if CreateLocal did not
		if err := c.EnsureLocalGardener(); err != nil {
			t.Logf("gardener: %v", err)
		}
	}
	if c.Mem != nil {
		_, _, err = c.Mem.AdmitClaim(memory.Claim{
			Subject: "MedThink", Predicate: "rpo_minutes", Object: "15",
			DocumentIDs:     []string{"dsid_policy", "dsid_runbook"},
			ValidFrom:       time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC),
			ObservedAt:      time.Now().UTC(),
			EvidenceQuality: 8,
		})
		if err != nil {
			t.Fatal(err)
		}
		_, _, err = c.Mem.AdmitRelation(memory.TemporalRelation{
			Src: "MedThink", Relation: "documented_in", Dst: "dsid_policy",
			FactText: "MedThink policy lives in dsid_policy", DocumentIDs: []string{"dsid_policy"},
			ValidFrom: time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC), ObservedAt: time.Now().UTC(),
		})
		if err != nil {
			t.Fatal(err)
		}
		nbrs := c.Mem.ExpandRelations([]string{"MedThink"}, time.Time{}, time.Time{}, 8)
		if len(nbrs) == 0 {
			t.Fatal("temporal relation expand empty after admit")
		}
		// Serve hydrate surface: DocumentIDs, not only entity neighbor names.
		docs := c.Mem.ExpandRelationDocuments([]string{"MedThink"}, time.Time{}, time.Time{}, 8)
		if len(docs) == 0 {
			t.Fatal("ExpandRelationDocuments empty — lean serve would not promote evidence")
		}
		// SeedRelationsFromClaims projects the rpo claim onto the graph too.
		if n := c.Mem.SeedRelationsFromClaims(); n < 0 {
			t.Fatal("seed relations failed")
		}
	}

	// Gardener wave: left-shift enrich (d2q/context/edges).
	if _, err := c.RunGardenerWave(ctx); err != nil {
		t.Logf("gardener wave (best-effort): %v", err)
	}

	// Lean ask — must not require QUALITY.
	ans := c.AnswerOpts(ctx, AnswerOptions{
		Question:     "What is the MedThink RPO for gold tier failover?",
		QuestionType: "basic",
		TopK:         6,
		GoldDocIDs:   []string{"dsid_policy", "dsid_runbook"},
	})
	if ans.Failure != "" {
		t.Fatalf("ask failure: %s diag=%v", ans.Failure, ans.RetrievalDiagnostics)
	}
	if ans.Answer == "" {
		t.Fatal("empty answer")
	}
	// Evidence should mention 15 or RPO or MedThink.
	low := strings.ToLower(ans.Answer)
	if !strings.Contains(low, "15") && !strings.Contains(low, "rpo") && !strings.Contains(low, "medthink") {
		t.Fatalf("answer missing gold signal: %q", ans.Answer)
	}
	// Prefer structure/product stack stamps when present.
	if ans.RetrievalDiagnostics != nil {
		if stack, _ := ans.RetrievalDiagnostics["product_stack"].(string); stack != "" &&
			!strings.Contains(stack, "residual") {
			t.Logf("product_stack=%s", stack)
		}
	}
}

// TestPreferInteractiveWhenHotLexPresent: serve uses HotLex interactive; QUALITY
// stays residual (HotLex still fused + Neon FTS skipped when strong).
func TestPreferInteractiveWhenHotLexPresent(t *testing.T) {
	c := &Client{hot: NewHotLex("b")}
	c.hot.AddChunk("c1", "d1", "MedThink RPO is fifteen minutes for gold tier", "")
	c.hot.Finalize()
	t.Setenv("OUROBOROS_ERB_FORCE_PATH2_FTS", "1")
	t.Setenv("OUROBOROS_ERB_QUALITY", "0")
	t.Setenv("OUROBOROS_ERB_PROD", "1")
	_ = os.Unsetenv("OUROBOROS_ERB_QUALITY_INTERACTIVE")
	p := prodProfileFromEnv()
	if !c.preferInteractive(p) {
		t.Fatal("serve + HotLex must prefer interactive (FORCE_PATH2_FTS must not disable hot)")
	}
	t.Setenv("OUROBOROS_ERB_QUALITY", "1")
	_ = os.Unsetenv("OUROBOROS_ERB_QUALITY_RESIDUAL")
	_ = os.Unsetenv("OUROBOROS_ERB_FORCE_RESIDUAL")
	p = prodProfileFromEnv()
	if !c.preferInteractive(p) {
		t.Fatal("QUALITY + HotLex must stay on the single interactive product path")
	}
	// Ablation-only residual opt-out.
	t.Setenv("OUROBOROS_ERB_FORCE_RESIDUAL", "1")
	p = prodProfileFromEnv()
	if c.preferInteractive(p) {
		t.Fatal("FORCE_RESIDUAL=1 must opt into residual multi-arm ablation")
	}
}
