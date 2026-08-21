package scmbench_test

import (
	"context"
	"testing"

	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/scmbench"
)

// fixtureRoot is the checked-in qafixture corpus, relative to this package.
const fixtureRoot = "testdata/qafixture"

func runQAFixture(t *testing.T) scmbench.QAReport {
	t.Helper()
	root := fixtureRoot
	cache := t.TempDir()
	suite := scmbench.QAFixtureSuite(root, cache)
	rep, err := scmbench.RunQA(context.Background(), suite)
	if err != nil {
		t.Fatalf("RunQA: %v", err)
	}
	if err := rep.MeasureQABaseline(root, suite.Queries); err != nil {
		t.Fatalf("MeasureQABaseline: %v", err)
	}
	return rep
}

func TestQAHitRatesOnCheckedInFixture(t *testing.T) {
	t.Parallel()
	rep := runQAFixture(t)

	if len(rep.Queries) == 0 {
		t.Fatal("no queries measured")
	}
	// Exact identifiers must be retrievable; the suite as a whole must hit at
	// top-10 for every probe on the local heuristic lane.
	if rep.HitRateAt10 < 1.0 {
		for _, q := range rep.Queries {
			if !q.HitAt10 {
				t.Errorf("query %q missed top-10: %+v", q.Name, q)
			}
		}
		t.Fatalf("hit@10=%.2f want 1.0", rep.HitRateAt10)
	}
	if rep.HitRateAt1 < 0.5 {
		t.Fatalf("hit@1=%.2f too low for exact identifiers", rep.HitRateAt1)
	}
	if rep.MeanPrecision <= 0 {
		t.Fatalf("mean precision=%v want >0", rep.MeanPrecision)
	}
	// No probe may error on a valid index.
	for _, q := range rep.Queries {
		if q.Failure == "error" {
			t.Fatalf("query %q errored: %+v", q.Name, q)
		}
	}
}

func TestQAReportDeterministicDigest(t *testing.T) {
	t.Parallel()
	a := runQAFixture(t)
	b := runQAFixture(t)
	if a.Digest == "" {
		t.Fatal("empty digest")
	}
	if a.Digest != b.Digest {
		t.Fatalf("digest not deterministic across runs:\n%s\n%s", a.Digest, b.Digest)
	}
	if a.HitRateAt1 != b.HitRateAt1 || a.MeanPrecision != b.MeanPrecision {
		t.Fatalf("hit/precision not deterministic: %+v vs %+v", a, b)
	}
}

func TestQATokenSavingsPositive(t *testing.T) {
	t.Parallel()
	rep := runQAFixture(t)
	if rep.BaselineBytes <= 0 {
		t.Fatalf("baseline bytes=%d", rep.BaselineBytes)
	}
	if rep.SavedTokensEst <= 0 || rep.TokenSavingsRatioEst <= 0 {
		t.Fatalf("expected token savings: %+v", rep)
	}
}

func TestQAThresholdGate(t *testing.T) {
	t.Parallel()
	rep := runQAFixture(t)
	rep.CheckThresholds(scmbench.QAFixtureThresholds())
	if !rep.Pass {
		t.Fatalf("baseline thresholds should pass on the checked-in fixture: %v", rep.Reasons)
	}

	// An impossible threshold must fail the gate and surface a reason.
	impossible := scmbench.QAFixtureThresholds()
	impossible.MinHitRateAt1 = 1.01
	rep.CheckThresholds(impossible)
	if rep.Pass {
		t.Fatal("unreachable hit@1 threshold must fail")
	}
	if len(rep.Reasons) == 0 {
		t.Fatal("failed gate must report reasons")
	}
}

func TestQAFailureClassification(t *testing.T) {
	t.Parallel()
	root := fixtureRoot
	cache := t.TempDir()
	suite := scmbench.QAFixtureSuite(root, cache)
	// Add a probe that cannot match anything → classified as empty/miss.
	suite.Queries = append(suite.Queries, scmbench.QAQuery{
		Name: "no-such-symbol", Q: "zzDefinitelyNotASymbolZZ", Expect: []string{"nope/none.go"},
	})
	rep, err := scmbench.RunQA(context.Background(), suite)
	if err != nil {
		t.Fatal(err)
	}
	var classified bool
	for _, q := range rep.Queries {
		if q.Name == "no-such-symbol" && (q.Failure == "miss" || q.Failure == "empty") {
			classified = true
		}
	}
	if !classified {
		t.Fatalf("unmatchable probe was not classified: %+v", rep.Failures)
	}
	if rep.Failures["miss"]+rep.Failures["empty"] == 0 {
		t.Fatalf("failure counts missing: %+v", rep.Failures)
	}
}

func TestQALaneDeclared(t *testing.T) {
	t.Parallel()
	rep := runQAFixture(t)
	if rep.Lane != scmbench.LaneLocalHeuristic {
		t.Fatalf("lane=%q want %q", rep.Lane, scmbench.LaneLocalHeuristic)
	}
	if scmbench.LaneDenseReranked == "" || scmbench.LaneCompilerAuthority == "" {
		t.Fatal("deferred lane constants must exist for honest claims")
	}
}
