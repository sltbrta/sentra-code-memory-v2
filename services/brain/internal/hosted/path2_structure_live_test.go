package hosted

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

// Live path2 structure consume smoke (S4 gate). Skips without Neon URL.
// Run:
//
//	set -a; source ~/.ouroboros/modal-smf-keys.env; set +a
//	go test ./services/brain/internal/hosted/ -count=1 -run Path2StructureLive -timeout 90s
//
// Not full-500 — single structure expand + optional lean retrieve diag stamp.
func TestPath2StructureLiveExpand(t *testing.T) {
	if strings.TrimSpace(os.Getenv("NEON_DATABASE_URL")) == "" &&
		strings.TrimSpace(os.Getenv("DATABASE_URL")) == "" {
		t.Skip("no NEON_DATABASE_URL/DATABASE_URL — path2 live smoke skipped")
	}
	cfg, err := FromEnv()
	if err != nil {
		t.Skipf("hosted FromEnv: %v", err)
	}
	c, err := Open(cfg)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer c.Close()
	if c.db == nil {
		t.Fatal("path2 client has nil db")
	}
	if c.productOwned {
		t.Fatal("OpenFromEnv/Open must not be productOwned")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	// Live SMF corpus uses HealthBridge / RPO language (not synthetic MedThink).
	q := "What is the HealthBridge RPO target?"
	docs, diag := path2StructureExpand(ctx, c.db, cfg.BrainID, q, nil, 12)
	mode, _ := diag["structure_mode"].(string)
	if mode != "path2_sql" {
		t.Fatalf("want structure_mode=path2_sql got %q diag=%v", mode, diag)
	}
	t.Logf("path2 structure: docs=%d entities=%v facts=%v rels=%v",
		len(docs), diag["path2_entities_hits"], diag["path2_facts_hits"], diag["path2_relationships_hits"])
	if e, _ := diag["path2_entities_error"].(string); e != "" {
		t.Fatalf("entities_error=%s", e)
	}
	// Facts may soft-timeout on 4.6M rows; entity arm is the S4 primary gate.
	if e, _ := diag["path2_facts_error"].(string); e != "" {
		t.Logf("facts_error (soft)=%s", e)
	}
	// Hard S4 gate: at least one arm returns document IDs after SMF schema align.
	if len(docs) < 1 {
		t.Fatalf("want path2 structure docs≥1 after SMF schema align; diag=%v", diag)
	}

	// Lean serve retrieve: structure SQL arm must stamp path2 diagnostics.
	t.Setenv("OUROBOROS_ERB_PROD", "1")
	t.Setenv("OUROBOROS_ERB_QUALITY", "0")
	t.Setenv("OUROBOROS_ERB_FORCE_PATH2_FTS", "0")
	rctx, rcancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer rcancel()
	passages, rdiag, rerr := c.RetrieveOpts(rctx, q, RetrieveOptions{
		QuestionType: "basic",
		TopK:         6,
	})
	if rerr != nil {
		t.Fatalf("Retrieve: %v", rerr)
	}
	if len(passages) == 0 {
		t.Log("Retrieve returned 0 passages (possible cold dense/lex timeout) — structure expand still gated")
	}
	// Hard S4 consume gate: retrieve must stamp path2_sql after detached structure budget.
	// (Pre-fix: parent-context starve left path2_unavailable while bare expand hit.)
	p2mode, _ := rdiag["path2_structure_mode"].(string)
	sMode, _ := rdiag["structure_mode"].(string)
	if p2mode != "path2_sql" && sMode != "path2_sql" {
		t.Fatalf("retrieve must consume path2 structure SQL; path2_structure_mode=%q structure_mode=%q budget_ms=%v diag keys plane=%v class=%v errors ent=%v fact=%v",
			p2mode, sMode, rdiag["structure_sql_budget_ms"], rdiag["plane"], rdiag["retrieve_class"],
			rdiag["path2_entities_error"], rdiag["path2_facts_error"])
	}
	promoted, _ := rdiag["structure_sql_promoted"].(int)
	if promoted < 1 {
		if f, ok := rdiag["structure_sql_promoted"].(float64); ok {
			promoted = int(f)
		}
	}
	if promoted < 1 {
		t.Fatalf("want structure_sql_promoted≥1 after path2_sql, got %v (docs expanded ok but hydrate starved?)", rdiag["structure_sql_promoted"])
	}
	t.Logf("retrieve structure stamps: path2_structure_mode=%q structure_mode=%q sql_promoted=%d budget_ms=%v",
		p2mode, sMode, promoted, rdiag["structure_sql_budget_ms"])
	// Plane honesty.
	if plane, _ := rdiag["plane"].(string); plane != "" && plane == "residual" {
		t.Fatalf("path2 Open must not stamp residual plane: %v", rdiag["plane"])
	}
}

// TestPath2StructureLiveTokenDensity probes a few high-signal ERB-ish tokens.
// Skips without Neon. Records which token classes hit entities/facts.
func TestPath2StructureLiveTokenDensity(t *testing.T) {
	if strings.TrimSpace(os.Getenv("NEON_DATABASE_URL")) == "" &&
		strings.TrimSpace(os.Getenv("DATABASE_URL")) == "" {
		t.Skip("no NEON — token density smoke skipped")
	}
	cfg, err := FromEnv()
	if err != nil {
		t.Skipf("FromEnv: %v", err)
	}
	c, err := Open(cfg)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer c.Close()

	queries := []string{
		"HealthBridge RPO",
		"CinderPoint SaaSWorks",
		"Redwood",
	}
	hits := 0
	for _, q := range queries {
		qctx, qcancel := context.WithTimeout(context.Background(), 12*time.Second)
		docs, diag := path2StructureExpand(qctx, c.db, cfg.BrainID, q, nil, 8)
		qcancel()
		mode, _ := diag["structure_mode"].(string)
		if mode != "path2_sql" {
			t.Errorf("q=%q mode=%q diag=%v", q, mode, diag)
			continue
		}
		n := len(docs)
		if n > 0 {
			hits++
		}
		t.Logf("q=%q docs=%d ent=%v fact=%v", q, n, diag["path2_entities_hits"], diag["path2_facts_hits"])
	}
	if hits == 0 {
		t.Fatal("zero structure docs across probe queries after SMF schema align")
	}
}

// S5 live subset: structure arm hit rate on retrieve (not full-500).
// Hard gate: ≥60% of probes stamp path2_sql with structure_sql_promoted≥1.
func TestPath2StructureLiveSubsetHitRate(t *testing.T) {
	if strings.TrimSpace(os.Getenv("NEON_DATABASE_URL")) == "" &&
		strings.TrimSpace(os.Getenv("DATABASE_URL")) == "" {
		t.Skip("no NEON — subset hit-rate skipped")
	}
	cfg, err := FromEnv()
	if err != nil {
		t.Skipf("FromEnv: %v", err)
	}
	c, err := Open(cfg)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer c.Close()

	t.Setenv("OUROBOROS_ERB_PROD", "1")
	t.Setenv("OUROBOROS_ERB_QUALITY", "0")
	t.Setenv("OUROBOROS_ERB_FORCE_PATH2_FTS", "0")

	// Small representative subset (S5 — not 40Q/full-500).
	queries := []string{
		"What is the HealthBridge RPO target?",
		"CinderPoint SaaSWorks architecture",
		"Redwood recovery policy",
		"HealthBridge warm replication",
		"SaaSWorks entity relationships",
	}
	hit := 0
	for _, q := range queries {
		rctx, cancel := context.WithTimeout(context.Background(), 50*time.Second)
		_, rdiag, rerr := c.RetrieveOpts(rctx, q, RetrieveOptions{QuestionType: "basic", TopK: 6})
		cancel()
		if rerr != nil {
			t.Logf("q=%q retrieve err=%v", q, rerr)
			continue
		}
		p2mode, _ := rdiag["path2_structure_mode"].(string)
		sMode, _ := rdiag["structure_mode"].(string)
		promoted, _ := rdiag["structure_sql_promoted"].(int)
		if promoted < 1 {
			if f, ok := rdiag["structure_sql_promoted"].(float64); ok {
				promoted = int(f)
			}
		}
		ok := (p2mode == "path2_sql" || sMode == "path2_sql") && promoted >= 1
		if ok {
			hit++
		}
		t.Logf("q=%q path2=%q mode=%q promoted=%d ok=%v", q, p2mode, sMode, promoted, ok)
	}
	rate := float64(hit) / float64(len(queries))
	t.Logf("structure_arm_hit_rate=%.2f (%d/%d)", rate, hit, len(queries))
	if rate < 0.60 {
		t.Fatalf("S5 subset structure arm hit rate %.2f < 0.60", rate)
	}
}
