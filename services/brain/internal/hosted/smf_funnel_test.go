package hosted

import (
	"testing"
	"time"
)

func TestBudgetsForFunnelLeanFast(t *testing.T) {
	b := budgetsForFunnel(TierLean, "basic", false, false)
	if b.DenseQueries > 2 {
		t.Fatalf("lean dense=%d", b.DenseQueries)
	}
	if b.CEPool > 48 {
		t.Fatalf("lean CE too wide: %d", b.CEPool)
	}
	if b.DenseRecovery || b.WholeDocN > 0 || b.CompletenessRefine {
		t.Fatal("lean must not pay expand tax")
	}
}

func TestBudgetsForFunnelExpandWide(t *testing.T) {
	b := budgetsForFunnel(TierExpand, "project_related", true, true)
	if b.CEPool < 100 {
		t.Fatalf("expand CE too narrow: %d (smf rerank_cap spirit)", b.CEPool)
	}
	if b.WholeDocN < 10 {
		t.Fatalf("expand whole-doc n=%d want ≥10", b.WholeDocN)
	}
	if !b.DenseRecovery {
		t.Fatal("expand must enable dense HyDE recovery")
	}
	if b.WholeDocBudget < 3*time.Second {
		t.Fatalf("whole-doc budget %v", b.WholeDocBudget)
	}
}

func TestShortHydeForDenseIsCompact(t *testing.T) {
	hy := shortHydeForDense("What is the MedThink RPO failover policy for gold tier datasets under multi-region active-active?")
	if hy == "" {
		t.Fatal("expected hyde")
	}
	if len(hy) > 450 {
		t.Fatalf("hyde too long for dense embed: %d", len(hy))
	}
}
