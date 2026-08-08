package main

import (
	"testing"

	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/chunking"
)

// TestRunEndToEnd exercises the full harness on the committed golden set.
func TestRunEndToEnd(t *testing.T) {
	docs, queries := GenerateFixtures()
	report, err := Run(ToSourceDocuments(docs), queries, chunking.EvalStrategies(), 8, false)
	if err != nil {
		t.Fatal(err)
	}

	if report.ScoreClass != "diagnostic" || report.Official {
		t.Fatalf("chunk-eval must only emit diagnostics: class=%s official=%v",
			report.ScoreClass, report.Official)
	}
	if report.Fixtures.Queries < minGoldenQueries {
		t.Fatalf("report measured %d queries, want >= %d", report.Fixtures.Queries, minGoldenQueries)
	}
	if len(report.Strategies) != len(chunking.EvalStrategies()) {
		t.Fatalf("report has %d strategies, want %d", len(report.Strategies), len(chunking.EvalStrategies()))
	}

	byStrategy := map[string]StrategyResult{}
	for _, s := range report.Strategies {
		byStrategy[s.Strategy] = s
		if s.CitationIntegrity != 1.0 {
			t.Errorf("strategy %s citation integrity = %v, want 1.0 (violations: %+v)",
				s.Strategy, s.CitationIntegrity, s.IntegrityViolations)
		}
		if s.HitRateAtK < 0.9 {
			t.Errorf("strategy %s hit rate %v unexpectedly low; harness or fixtures broken",
				s.Strategy, s.HitRateAtK)
		}
		if s.MRR <= 0 || s.MRR > 1 || s.NDCGAtK <= 0 || s.NDCGAtK > 1 {
			t.Errorf("strategy %s metrics out of range: mrr=%v ndcg=%v", s.Strategy, s.MRR, s.NDCGAtK)
		}
		if s.ChunksIndexed == 0 || s.IndexTokens == 0 {
			t.Errorf("strategy %s cost block empty", s.Strategy)
		}
		if s.Policy == nil || s.Policy["policy_version"] == nil {
			t.Errorf("strategy %s must stamp its policy fingerprint in non-blind mode", s.Strategy)
		}
	}

	// The naive baseline indexes exactly one chunk per document.
	if got := byStrategy["whole_doc"].ChunksIndexed; got != len(docs) {
		t.Errorf("whole_doc indexed %d chunks, want one per document (%d)", got, len(docs))
	}
	// Every windowed strategy must expand the index beyond source volume
	// (overlap) and produce more chunks than documents.
	for _, name := range []string{"fixed", "structure", "parent_child"} {
		s := byStrategy[name]
		if s.ChunksIndexed <= len(docs) {
			t.Errorf("%s produced %d chunks for %d docs; expected chunk growth", name, s.ChunksIndexed, len(docs))
		}
		if s.ExpansionRatio < 1.0 {
			t.Errorf("%s expansion ratio %v < 1", name, s.ExpansionRatio)
		}
	}
	// Parent-child keeps parents out of the index but in the ledger.
	pc := byStrategy["parent_child"]
	if pc.ChunksTotal <= pc.ChunksIndexed {
		t.Errorf("parent_child ledger %d should exceed index %d (parents)", pc.ChunksTotal, pc.ChunksIndexed)
	}
}

func TestRunBlindModeHidesFingerprints(t *testing.T) {
	docs, queries := GenerateFixtures()
	report, err := Run(ToSourceDocuments(docs), queries, chunking.EvalStrategies(), 4, true)
	if err != nil {
		t.Fatal(err)
	}
	if !report.ERBBlind {
		t.Fatal("blind run must stamp erb_blind")
	}
	seenLabels := map[string]bool{}
	for _, s := range report.Strategies {
		if s.Policy != nil {
			t.Errorf("blind report leaks policy fingerprint for %s", s.Strategy)
		}
		if s.BlindLabel == "" {
			t.Errorf("blind report missing arm label")
		}
		if seenLabels[s.BlindLabel] {
			t.Errorf("duplicate blind label %s", s.BlindLabel)
		}
		seenLabels[s.BlindLabel] = true
	}
}
