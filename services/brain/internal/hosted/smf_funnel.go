package hosted

import (
	"strings"
	"time"
)

// smf_funnel.go — latency-tiered Path-4 style budgets (smf-research).
//
// Lean (majority): fast hybrid, tight CE/window, no recovery/whole-doc tax.
// Expand (weak/multi-gold): wide CE, HyDE/doc2query dense, multi-gold window,
// whole-doc hydrate (answer path), coverage MMR — still wall-capped.

// funnelBudgets holds per-ask widths chosen from EvidenceTier + type.
type funnelBudgets struct {
	DenseQueries int
	CEPool       int
	WinK         int
	DivCap       int
	CovLambda    float64
	FactsLimit   int
	EntityMax    int
	// Recovery
	RecN           int
	DenseRecovery  bool // short HyDE dense even if phase-A had dense
	Doc2QueryN     int
	// Hydrate (answer)
	WholeDocN      int
	WholeDocChunks int
	WholeDocBudget time.Duration
	// Answer
	CompletenessRefine bool
	GroundingVerify    bool
}

// budgetsForFunnel maps tier + question shape to smf-style widths with latency caps.
func budgetsForFunnel(tier EvidenceTier, questionType string, multiGold bool, quality bool) funnelBudgets {
	b := funnelBudgets{
		DenseQueries:   2,
		CEPool:         48,
		WinK:           12,
		DivCap:         3,
		CovLambda:      0.72,
		FactsLimit:     4,
		EntityMax:      12,
		RecN:           0,
		DenseRecovery:  false,
		Doc2QueryN:     0,
		WholeDocN:      0,
		WholeDocChunks: 0,
		WholeDocBudget: 0,
	}
	multiDoc := isMultiDocType(questionType)
	switch tier {
	case TierLean:
		b.DenseQueries = 1
		if quality {
			b.DenseQueries = 2
		}
		b.CEPool = 40
		b.WinK = 12
		// No recovery / whole-doc / refine on lean.
		return b
	case TierStandard:
		b.DenseQueries = 2
		b.CEPool = 80
		b.WinK = 14
		b.FactsLimit = 6
		b.EntityMax = 16
		b.RecN = 4
		b.Doc2QueryN = 1
		b.WholeDocN = 6
		b.WholeDocChunks = 8
		b.WholeDocBudget = 2500 * time.Millisecond
		if multiDoc || multiGold {
			b.WinK = 16
			b.DivCap = 5
			b.CovLambda = 0.55 // more diversity
			b.WholeDocN = 10
			b.WholeDocChunks = 10
			b.WholeDocBudget = 3500 * time.Millisecond
			b.CompletenessRefine = true
		}
		return b
	default: // expand
		b.DenseQueries = 3
		b.CEPool = 120
		b.WinK = 18
		b.DivCap = 5
		b.CovLambda = 0.55
		b.FactsLimit = 8
		b.EntityMax = 24
		b.RecN = 6
		b.DenseRecovery = true // smf: +8pt HyDE dense on weak
		b.Doc2QueryN = 2
		b.WholeDocN = 10
		b.WholeDocChunks = 12
		b.WholeDocBudget = 3500 * time.Millisecond
		b.CompletenessRefine = true
		b.GroundingVerify = true
		if multiDoc || multiGold {
			b.CEPool = 150
			b.WinK = 20
			b.DivCap = 6
			b.WholeDocN = 12
			b.WholeDocChunks = 12
			b.WholeDocBudget = 4 * time.Second
		}
		if strings.EqualFold(questionType, "basic") && !multiGold {
			// Expand basic: still whole-doc but tighter wall.
			b.WholeDocN = 6
			b.WholeDocChunks = 8
			b.WholeDocBudget = 3 * time.Second
			b.CEPool = 100
			b.WinK = 14
		}
		return b
	}
}

// shortHydeForDense returns a compact HyDE string for embed (not HotLex).
// Long HyDE burned 11–14s on BM25; dense embed wants ≤512 chars.
func shortHydeForDense(question string) string {
	hy := hydeStub(question)
	if hy == "" {
		return ""
	}
	if len(hy) > 400 {
		// Prefer first sentence-ish of stub.
		hy = hy[:400]
	}
	// Prefer phrase head if stub is still too long.
	if len(hy) > 280 {
		if ph := pickHotLexPhrases(question, 2); len(ph) > 0 {
			return "Document describing " + strings.Join(ph, "; ") + "."
		}
	}
	return hy
}

// stampFunnelDiag records smf-style stage budget decisions for p95 attribution.
func stampFunnelDiag(diag map[string]any, tier EvidenceTier, b funnelBudgets) {
	if diag == nil {
		return
	}
	diag["smf_funnel"] = true
	diag["funnel_tier"] = tier.String()
	diag["funnel_dense_queries"] = b.DenseQueries
	diag["funnel_ce_pool"] = b.CEPool
	diag["funnel_win_k"] = b.WinK
	diag["funnel_whole_doc_n"] = b.WholeDocN
	diag["funnel_dense_recovery"] = b.DenseRecovery
	diag["funnel_completeness_refine"] = b.CompletenessRefine
	diag["funnel_grounding_verify"] = b.GroundingVerify
}
