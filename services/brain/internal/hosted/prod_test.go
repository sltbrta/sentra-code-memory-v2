package hosted

import (
	"os"
	"testing"
	"time"
)

func TestProdProfileDefaultFast(t *testing.T) {
	_ = os.Unsetenv("OUROBOROS_ERB_QUALITY")
	_ = os.Unsetenv("OUROBOROS_ERB_BENCHMAX")
	_ = os.Unsetenv("OUROBOROS_ERB_BENCH_MAX")
	_ = os.Unsetenv("OUROBOROS_ERB_MODE")
	_ = os.Unsetenv("OUROBOROS_ERB_PROD")
	_ = os.Unsetenv("OUROBOROS_ERB_DENSE_QUERIES")
	_ = os.Unsetenv("OUROBOROS_ERB_HOSTED_POOL_K")
	_ = os.Unsetenv("OUROBOROS_BRAIN_AGENTIC")
	_ = os.Unsetenv("OUROBOROS_ERB_LEX_TIMEOUT_MS")
	p := prodProfileFromEnv()
	if !p.Enabled {
		t.Fatal("prod should default on")
	}
	// SOTA product: dense always on (not 1-query lean that skips hybrid).
	if p.DenseQueries < 2 {
		t.Fatalf("dense queries=%d want ≥2 for hybrid product path", p.DenseQueries)
	}
	if p.PoolK < 48 {
		t.Fatalf("poolK=%d too small for CE union (SOTA needs ≥48)", p.PoolK)
	}
	if !p.Agentic {
		t.Fatal("agentic default on — ExpandLite + signal gate (not latency kill-switch)")
	}
	if !p.StructurePoolOnly {
		t.Fatal("structure should be pool-only in prod")
	}
	if p.Corrective {
		t.Fatal("corrective should be off in prod")
	}
	// Light: ≤2s arms for 30s wall.
	if p.LexTimeout > 2500*time.Millisecond {
		t.Fatalf("light lex timeout too large: %v (want ≤2.5s)", p.LexTimeout)
	}
}

func TestQualityProfileDisablesProd(t *testing.T) {
	t.Setenv("OUROBOROS_ERB_QUALITY", "1")
	t.Setenv("OUROBOROS_ERB_BENCHMAX", "0")
	_ = os.Unsetenv("OUROBOROS_ERB_LEX_TIMEOUT_MS")
	_ = os.Unsetenv("OUROBOROS_ERB_DENSE_QUERIES")
	_ = os.Unsetenv("OUROBOROS_ERB_STRUCTURE_POOL_ONLY")
	p := prodProfileFromEnv()
	if p.Enabled {
		t.Fatal("quality mode should disable prod caps")
	}
	if !p.Quality {
		t.Fatal("quality flag")
	}
	if p.DenseQueries < 2 {
		t.Fatalf("quality dense=%d", p.DenseQueries)
	}
	// Warm p50 ≤30s: QUALITY arms ≤3s.
	if p.LexTimeout > 3*time.Second {
		t.Fatalf("quality lex timeout too large: %v (want ≤3s for warm p50≤30s)", p.LexTimeout)
	}
	if structureSQLBudget(p) > 2500*time.Millisecond {
		t.Fatalf("quality structure SQL budget too large: %v", structureSQLBudget(p))
	}
	if denseBudget(p) > 3*time.Second {
		t.Fatalf("quality dense budget too large: %v", denseBudget(p))
	}
	if !p.StructurePoolOnly {
		t.Fatal("quality default StructurePoolOnly — skip Neon FTS structure rescue")
	}
	if p.Corrective {
		t.Fatal("quality default corrective off (second retrieve doubles wall)")
	}
}

func TestBenchmaxProfileUnbounded(t *testing.T) {
	t.Setenv("OUROBOROS_ERB_BENCHMAX", "1")
	t.Setenv("OUROBOROS_ERB_QUALITY", "1")
	t.Setenv("OUROBOROS_ERB_PROD", "1")
	t.Setenv("OUROBOROS_ERB_LEAN", "1")
	p := prodProfileFromEnv()
	if p.Enabled || !p.Quality || !p.Benchmax {
		t.Fatalf("benchmax precedence/profile invalid: %#v", p)
	}
	if p.LexTimeout != 0 || p.StructurePoolOnly {
		t.Fatalf("benchmax must be unbounded and allow structure rescue: %#v", p)
	}
	if p.DenseQueries < 4 || p.PoolK < 48 || p.MaxSynthRetry < 3 {
		t.Fatalf("benchmax budgets too small: %#v", p)
	}
	if p.AgenticRounds < 4 || p.AgenticExtra < 8 || !p.Agentic || !p.Corrective {
		t.Fatalf("benchmax answer budgets invalid: %#v", p)
	}
	if leanMode() {
		t.Fatal("benchmax must force lean mode off")
	}

	diag := map[string]any{}
	stampQualityProfile(diag, p)
	if diag["quality_profile"] != "benchmax" || diag["benchmax"] != true ||
		diag["latency_budget"] != "none" || diag["cost_budget"] != "none" {
		t.Fatalf("benchmax diagnostics missing: %#v", diag)
	}
}

func TestBenchmaxAliases(t *testing.T) {
	t.Setenv("OUROBOROS_ERB_BENCHMAX", "0")
	t.Setenv("OUROBOROS_ERB_BENCH_MAX", "1")
	t.Setenv("OUROBOROS_ERB_MODE", "")
	if !prodProfileFromEnv().Benchmax {
		t.Fatal("BENCH_MAX alias should enable benchmax")
	}
	t.Setenv("OUROBOROS_ERB_BENCH_MAX", "0")
	t.Setenv("OUROBOROS_ERB_MODE", "bench")
	if prodProfileFromEnv().Benchmax {
		t.Fatal("mode=bench must remain bounded bench, not benchmax")
	}
}

func TestWantsAgenticBenchmaxBroader(t *testing.T) {
	t.Setenv("OUROBOROS_ERB_BENCHMAX", "1")
	t.Setenv("OUROBOROS_ERB_QUALITY", "0")
	t.Setenv("OUROBOROS_ERB_MODE", "lean")
	for _, qt := range []string{"project_related", "completeness", "conflicting_info", "semantic", "constrained", "intra_document_reasoning"} {
		if !wantsAgentic(qt) {
			t.Fatalf("benchmax should enable agentic for %q", qt)
		}
	}
	if wantsAgentic("basic") || wantsAgentic("info_not_found") {
		t.Fatal("benchmax must not agentically expand basic/info_not_found")
	}
}

func TestPhaseABudgetExpandLiteTight(t *testing.T) {
	t.Setenv("OUROBOROS_ERB_QUALITY", "1")
	_ = os.Unsetenv("OUROBOROS_ERB_LEX_TIMEOUT_MS")
	_ = os.Unsetenv("OUROBOROS_ERB_DENSE_TIMEOUT_MS")
	p := prodProfileFromEnv()
	if b := phaseABudget(p, true); b > 1500*time.Millisecond {
		t.Fatalf("ExpandLite phaseA budget %v want ≤1.5s", b)
	}
	if b := phaseABudget(p, false); b > 3*time.Second {
		t.Fatalf("QUALITY phaseA budget %v want ≤3s", b)
	}
}

func TestPath2StructureArmBudgetsSplit(t *testing.T) {
	e, tail := path2StructureArmBudgets(12 * time.Second)
	if e <= 0 || tail <= 0 {
		t.Fatalf("e=%v tail=%v", e, tail)
	}
	if e >= 12*time.Second {
		t.Fatalf("entity must not consume full budget: e=%v", e)
	}
	if e+tail < 12*time.Second {
		t.Logf("e=%v tail=%v", e, tail)
	}
	if e < 1500*time.Millisecond || tail < 1500*time.Millisecond {
		t.Fatalf("floors: e=%v tail=%v", e, tail)
	}
}
