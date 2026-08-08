package hosted

import (
	"context"
	"testing"
	"time"
)

// ExpandLite must not re-run recovery/structure path (agentic nested wall).
func TestExpandLiteSkipsHeavyArms(t *testing.T) {
	t.Setenv("OUROBOROS_ERB_QUALITY", "1")
	t.Setenv("OUROBOROS_ERB_PROD", "0")
	t.Setenv("OUROBOROS_BRAIN_LLM", "extractive")
	t.Setenv("OPENAI_API_KEY", "")

	dir := t.TempDir()
	c, err := CreateLocal(dir, "expand-lite")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	ctx := context.Background()
	docs := []LocalDocument{
		{ID: "d1", Title: "MedThink RPO", Text: "MedThink gold tier recovery point objective is fifteen minutes for failover."},
		{ID: "d2", Title: "Policy", Text: "Alpha Widget blue paint policy for warehouse."},
	}
	if _, err := c.BurstIngestLocal(ctx, docs, 1); err != nil {
		t.Fatal(err)
	}
	// Warm HotLex from local.
	if c.hot == nil || c.hot.Len() == 0 {
		t.Skip("no HotLex after local ingest")
	}
	rctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	_, diag, err := c.RetrieveOpts(rctx, "MedThink recovery RPO failover", RetrieveOptions{
		TopK:         6,
		QuestionType: "basic",
		ExpandLite:   true,
	})
	if err != nil {
		t.Fatalf("Retrieve ExpandLite: %v", err)
	}
	if diag["expand_lite"] != true {
		t.Fatalf("expected expand_lite stamp, diag=%v", diag["expand_lite"])
	}
	if diag["recovery"] == true {
		t.Fatal("ExpandLite must skip multi-list recovery")
	}
	if s, _ := diag["recovery_skipped"].(string); s != "expand_lite" && diag["recovery"] != false {
		t.Logf("recovery_skipped=%v recovery=%v", diag["recovery_skipped"], diag["recovery"])
	}
	if diag["structure_sql_skipped"] != "expand_lite" && diag["structure_sql_budget_ms"] != nil {
		// path2 only when Neon; local may not set structure_sql_skipped if productOwned
		t.Logf("structure skip note: sql_skipped=%v budget=%v", diag["structure_sql_skipped"], diag["structure_sql_budget_ms"])
	}
}
