package hosted

import (
	"context"
	"database/sql"
	"testing"
	"time"
)

func TestPath2StructureFromRowsMerge(t *testing.T) {
	out, diag := path2StructureFromRows(
		[]string{"e1", "e2", "e1"},
		[]string{"f1", "e2"},
		[]string{"r1", "e1"},
		4,
	)
	if len(out) != 4 {
		t.Fatalf("want 4 unique capped docs got %v diag=%v", out, diag)
	}
	// Order: entities then facts then rels, deduped.
	if out[0] != "e1" || out[1] != "e2" {
		t.Fatalf("entity-first order: %v", out)
	}
	if diag["path2_entities_hits"] != 3 {
		t.Fatalf("entities_hits=%v", diag["path2_entities_hits"])
	}
	if diag["path2_facts_hits"] != 2 {
		t.Fatalf("facts_hits=%v", diag["path2_facts_hits"])
	}
	if diag["path2_relationships_hits"] != 2 {
		t.Fatalf("rel_hits=%v", diag["path2_relationships_hits"])
	}
	if diag["path2_structure_docs"] != 4 {
		t.Fatalf("structure_docs=%v", diag["path2_structure_docs"])
	}
	if diag["structure_mode"] != "path2_sql" {
		t.Fatalf("mode=%v", diag["structure_mode"])
	}
	// maxN=2 caps early.
	out2, d2 := path2StructureFromRows([]string{"a", "b", "c"}, nil, nil, 2)
	if len(out2) != 2 || d2["path2_structure_docs"] != 2 {
		t.Fatalf("cap: %v %#v", out2, d2)
	}
}

func TestPath2StructureExpandNilDB(t *testing.T) {
	docs, diag := path2StructureExpand(context.Background(), nil, "brain", "What is MedThink RPO?", []string{"s1"}, 8)
	if docs != nil {
		t.Fatalf("nil db must return nil docs: %v", docs)
	}
	if diag["structure_mode"] != "path2_unavailable" {
		t.Fatalf("want path2_unavailable got %#v", diag)
	}
	if diag["path2_structure_docs"] != 0 {
		t.Fatalf("docs count %#v", diag["path2_structure_docs"])
	}
}

// Merge must keep pool_virtual when path2 SQL is unavailable (nil db / all arms fail).
func TestMergePath2DiagKeepsPoolVirtualWhenUnavailable(t *testing.T) {
	diag := map[string]any{"structure_mode": "pool_virtual", "structure_neighbors": 2}
	p2docs, p2diag := path2StructureExpand(context.Background(), nil, "brain", "What is MedThink RPO?", []string{"s1"}, 8)
	if p2docs != nil {
		t.Fatalf("nil db docs: %v", p2docs)
	}
	if p2diag["structure_mode"] != "path2_unavailable" {
		t.Fatalf("path2 expand mode %#v", p2diag["structure_mode"])
	}
	mergePath2StructureDiag(diag, p2diag, len(p2docs))
	if diag["structure_mode"] != "pool_virtual" {
		t.Fatalf("want pool_virtual after merge, got %#v", diag["structure_mode"])
	}
	if diag["path2_structure_mode"] != "path2_unavailable" {
		t.Fatalf("want path2_structure_mode=path2_unavailable got %#v", diag["path2_structure_mode"])
	}
	if diag["path2_structure_docs"] != 0 {
		t.Fatalf("path2 docs count %#v", diag["path2_structure_docs"])
	}
	if diag["structure_neighbors"] != 2 {
		t.Fatalf("unrelated keys must survive merge: %#v", diag)
	}
}

func TestMergePath2DiagStampsPath2SQLOnHits(t *testing.T) {
	diag := map[string]any{"structure_mode": "pool_virtual"}
	p2diag := map[string]any{
		"structure_mode":       "path2_sql",
		"path2_structure_docs": 3,
		"path2_entities_hits":  3,
	}
	mergePath2StructureDiag(diag, p2diag, 3)
	if diag["structure_mode"] != "path2_sql" {
		t.Fatalf("want path2_sql got %#v", diag["structure_mode"])
	}
	if diag["path2_structure_mode"] != "path2_sql" {
		t.Fatalf("path2_structure_mode %#v", diag["path2_structure_mode"])
	}
}

func TestStructureTimeoutPropagatesCancelledParent(t *testing.T) {
	// Structure SQL belongs to the request and must not outlive cancellation.
	parent, cancel := context.WithCancel(context.Background())
	cancel()
	ctxInherit, c1 := withTimeout(parent, 2*time.Second)
	defer c1()
	if err := ctxInherit.Err(); err == nil {
		t.Fatal("inherited timeout should already be cancelled with dead parent")
	}
	// Latency-hardened prod default is 1.5s (QUALITY=2s); override via env.
	// This assertion only guards that a non-zero budget is wired — not a floor race.
	budget := structureSQLBudget(ProdProfile{Enabled: true})
	if budget < 500*time.Millisecond || budget > 5*time.Second {
		t.Fatalf("prod structure budget out of expected range: %v", budget)
	}
}

func TestPath2NearDeadlineBudgetIsDiagnosedHonestly(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	total, source, near := path2StructureBudget(ctx)
	if source != "caller_deadline" || !near || total <= 0 || total > 25*time.Millisecond {
		t.Fatalf("near deadline budget total=%v source=%q near=%v", total, source, near)
	}
	docs, diag := path2StructureExpand(ctx, nil, "brain", "Atlas recovery objective", []string{"seed"}, 4)
	if docs != nil {
		t.Fatalf("nil DB returned docs: %v", docs)
	}
	// Nil DB exits before budget work; exercise a live-shaped canceled context
	// so the budget provenance reaches diagnostics without starting SQL.
	canceled, stop := context.WithDeadline(context.Background(), time.Now().Add(-time.Millisecond))
	defer stop()
	_, diag = path2StructureExpand(canceled, &sql.DB{}, "brain", "Atlas recovery objective", []string{"seed"}, 4)
	if diag["path2_structure_budget_source"] != "caller_deadline" || diag["path2_structure_near_deadline"] != true ||
		diag["path2_structure_total_ms"] != int64(0) {
		t.Fatalf("near-deadline path2 diagnostics=%#v", diag)
	}
}

func TestPath2StructureEntityTokensTop6(t *testing.T) {
	// contentTokens drives entity token extract; long questions still feed expand.
	q := "What is the MedThink recovery failover RPO policy threshold for gold tier datasets?"
	toks := contentTokens(q)
	if len(toks) < 4 {
		t.Fatalf("expected content tokens for structure expand, got %v", toks)
	}
	// Expand with nil db still unavailable (no panic).
	_, diag := path2StructureExpand(context.Background(), nil, "b", q, []string{"seed"}, 6)
	if diag["structure_mode"] != "path2_unavailable" {
		t.Fatalf("%#v", diag)
	}
}

func TestOpenMemoryStructureModeNotPath2SQL(t *testing.T) {
	t.Setenv("OUROBOROS_ERB_QUALITY", "1")
	c := OpenMemory("no-path2-sql")
	defer c.Close()
	ctx := context.Background()
	_ = c.EnsureSchema(ctx)
	if err := c.UpsertChunks(ctx, "no-path2-sql", []ChunkWrite{
		{DocumentID: "s", ChunkID: "c", Text: "MedThink recovery policy Alpha PROJ_X for structure."},
	}); err != nil {
		t.Fatal(err)
	}
	_, diag, err := c.RetrieveOpts(ctx, "MedThink recovery policy Alpha", RetrieveOptions{
		TopK:         4,
		QuestionType: "project_related",
	})
	if err != nil {
		t.Fatal(err)
	}
	// productOwned residual never runs path2 SQL structure expand.
	if mode, _ := diag["structure_mode"].(string); mode == "path2_sql" {
		t.Fatalf("OpenMemory must not stamp path2_sql: %#v", diag["structure_mode"])
	}
	if c.ProductOwned() != true {
		t.Fatal("expected productOwned")
	}
}
