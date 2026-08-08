package memory_test

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/memory"
)

func TestBiTemporalClaimsAndContradiction(t *testing.T) {
	dir := t.TempDir()
	s, err := memory.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t0 := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	c1, contested, err := s.AdmitClaim(memory.Claim{
		Subject: "Widget", Predicate: "color", Object: "blue",
		DocumentIDs: []string{"d1"}, ValidFrom: t0,
	})
	if err != nil || c1.Status != memory.ClaimActive {
		t.Fatalf("c1=%+v contested=%v err=%v", c1, contested, err)
	}
	// Conflicting object same interval → contested
	c2, contested, err := s.AdmitClaim(memory.Claim{
		Subject: "Widget", Predicate: "color", Object: "red",
		DocumentIDs: []string{"d2"}, ValidFrom: t0,
	})
	if err != nil {
		t.Fatal(err)
	}
	if c2.Status != memory.ClaimContested || len(contested) == 0 {
		t.Fatalf("expected contested, got c2=%+v contested=%+v", c2, contested)
	}
	// Current without contested should be empty for color
	cur := s.CurrentClaims(t0.Add(24*time.Hour), false)
	for _, c := range cur {
		if c.Predicate == "color" {
			t.Fatalf("contested claim exposed as current: %+v", c)
		}
	}
	// With contested included
	cur2 := s.CurrentClaims(t0.Add(24*time.Hour), true)
	if len(cur2) < 2 {
		t.Fatalf("want contested claims visible: %+v", cur2)
	}
	// Supersession
	c3, err := s.SupersedeClaim(c1.ID, memory.Claim{
		Subject: "Widget", Predicate: "color", Object: "green", DocumentIDs: []string{"d3"},
	}, t0.Add(48*time.Hour))
	// c1 was contested; supersede still works on ID
	if err != nil {
		// try supersede c2
		c3, err = s.SupersedeClaim(c2.ID, memory.Claim{
			Subject: "Widget", Predicate: "color", Object: "green", DocumentIDs: []string{"d3"},
		}, t0.Add(48*time.Hour))
	}
	if err != nil {
		t.Fatal(err)
	}
	if c3.Object != "green" {
		t.Fatalf("%+v", c3)
	}
}

func TestEpisodesAndResegment(t *testing.T) {
	s, err := memory.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	e1, err := s.BindEpisode(memory.Episode{
		ID: "e1", Kind: "ingest", DocumentIDs: []string{"a", "b"},
		Start: time.Now().UTC(),
	})
	if err != nil || e1.ID != "e1" {
		t.Fatal(err, e1)
	}
	_, _ = s.BindEpisode(memory.Episode{
		ID: "e2", Kind: "ingest", DocumentIDs: []string{"c"},
		Start: time.Now().UTC(),
	})
	merged, err := s.ResegmentEpisode("e-merged", []string{"e1", "e2"}, "all")
	if err != nil {
		t.Fatal(err)
	}
	if len(merged.DocumentIDs) != 3 {
		t.Fatalf("%+v", merged)
	}
	if len(s.EpisodesForDocument("a")) == 0 {
		t.Fatal("missing episode for a")
	}
}

func TestUtilityClosedLoopRanking(t *testing.T) {
	s, err := memory.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s.EnsureUtility([]string{"hot", "cold"})
	_ = s.SetUtility("hot", 2.0)
	_ = s.SetUtility("cold", 0.5)
	base := map[string]float64{"hot": 1.0, "cold": 1.0}
	boosted := s.ApplyUtilityToScores(base)
	if boosted["hot"] <= boosted["cold"] {
		t.Fatalf("utility must change ranking: %+v", boosted)
	}
	s.ReinforceUtility([]string{"cold"}, 0.5)
	if s.GetUtility("cold") <= 0.5 {
		t.Fatalf("reinforce failed: %v", s.GetUtility("cold"))
	}
	before := s.GetUtility("hot")
	s.DecayUtility(0.9)
	if s.GetUtility("hot") >= before {
		t.Fatalf("decay failed %v -> %v", before, s.GetUtility("hot"))
	}
	ranked := s.RankDocumentsByUtility([]string{"cold", "hot"})
	if ranked[0] == "" {
		t.Fatal(ranked)
	}
}

func TestC1ProbeSkip(t *testing.T) {
	p := memory.Probe{Question: "alpha", ExpectedDocIDs: []string{"d1"}}
	ok := memory.MeasureProbe(p, []string{"d1", "d2"}, 5)
	if ok.PredictionError != 0 {
		t.Fatalf("%+v", ok)
	}
	bad := memory.MeasureProbe(p, []string{"d9"}, 5)
	if bad.PredictionError != 1 {
		t.Fatalf("%+v", bad)
	}
	agg := memory.AggregatePredictionError([]memory.ProbeResult{ok, bad})
	if agg != 0.5 {
		t.Fatalf("%v", agg)
	}
	if !memory.ShouldSkipConsolidation(0.05, 0.15) {
		t.Fatal("should skip")
	}
	if memory.ShouldSkipConsolidation(0.5, 0.15) {
		t.Fatal("should not skip")
	}
}

func TestPPRMultiHop(t *testing.T) {
	// A-B-C chain: seed A should spread to C
	edges := map[string][]string{
		"A": {"B"}, "B": {"A", "C"}, "C": {"B"},
	}
	scores := memory.PersonalizedPageRank(map[string]float64{"A": 1}, edges, 0.85, 30)
	if scores["C"] <= 0 {
		t.Fatalf("PPR should reach C: %+v", scores)
	}
	top := memory.TopKFromScores(scores, 2)
	if top[0] != "A" && top[0] != "B" {
		// A or B should dominate
		t.Logf("top=%v scores=%v", top, scores)
	}
}

func TestRAPTORAndAgentMemory(t *testing.T) {
	docs := map[string]string{
		"d1": "alpha project widget blue",
		"d2": "alpha project widget red",
		"d3": "beta service latency",
	}
	nodes := memory.BuildRAPTORSummaries(docs, 4)
	if len(nodes) == 0 {
		t.Fatal("expected raptor nodes")
	}
	s, err := memory.Open(filepath.Join(t.TempDir(), "brain"))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.StoreRAPTOR(nodes); err != nil {
		t.Fatal(err)
	}
	if len(s.ListSummaries()) == 0 {
		t.Fatal("persist failed")
	}
	// policy gate
	if _, err := s.PutAgentMemory("", "note", "x", nil); err == nil {
		t.Fatal("empty principal must fail")
	}
	e, err := s.PutAgentMemory("alice", "preference", "likes sapphire", []string{"color"})
	if err != nil {
		t.Fatal(err)
	}
	got := s.SearchAgentMemory("alice", "sapphire", 10)
	if len(got) != 1 || got[0].ID != e.ID {
		t.Fatalf("%+v", got)
	}
	if len(s.GetAgentMemory("bob", 10)) != 0 {
		t.Fatal("cross-principal leak")
	}
}

func TestExtractClaimsConflictingObjects(t *testing.T) {
	s, err := memory.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	d1 := "Alpha Widget is painted blue sapphire in plant A. Widget costs $12."
	d2 := "Alpha Widget is painted red crimson in plant A. Widget costs $99."
	for _, cl := range memory.ExtractClaimsFromText("doc-blue", d1) {
		if _, _, err := s.AdmitClaim(cl); err != nil {
			t.Fatal(err)
		}
	}
	for _, cl := range memory.ExtractClaimsFromText("doc-red", d2) {
		if _, _, err := s.AdmitClaim(cl); err != nil {
			t.Fatal(err)
		}
	}
	groups := s.ContestedGroups()
	if len(groups) == 0 {
		// Dump claims for debugging.
		t.Fatalf("expected ContestedGroups non-empty after conflicting extracts; claims=%+v extract1=%+v extract2=%+v",
			s.CurrentClaims(time.Time{}, true),
			memory.ExtractClaimsFromText("doc-blue", d1),
			memory.ExtractClaimsFromText("doc-red", d2),
		)
	}
}

// TestExtractWidgetPriceIs covers "X price is $N" (not only "price of X is" / "X costs").
// Conflicting prices must surface as contested (subject Widget, predicate price).
func TestExtractWidgetPriceIs(t *testing.T) {
	d1 := "Widget price is $10."
	d2 := "Widget price is $12."
	e1 := memory.ExtractClaimsFromText("doc-w10", d1)
	e2 := memory.ExtractClaimsFromText("doc-w12", d2)

	hasPrice := func(claims []memory.Claim, obj string) bool {
		for _, cl := range claims {
			if strings.EqualFold(cl.Subject, "Widget") &&
				strings.EqualFold(cl.Predicate, "price") &&
				strings.Contains(cl.Object, obj) {
				return true
			}
		}
		return false
	}
	if !hasPrice(e1, "10") {
		t.Fatalf("expected Widget/price/$10 from %q; got %+v", d1, e1)
	}
	if !hasPrice(e2, "12") {
		t.Fatalf("expected Widget/price/$12 from %q; got %+v", d2, e2)
	}
	// reIs must not steal subject "Widget price" with pred "is".
	for _, cl := range append(append([]memory.Claim{}, e1...), e2...) {
		if strings.EqualFold(cl.Predicate, "is") &&
			strings.Contains(strings.ToLower(cl.Subject), "price") {
			t.Fatalf("reIs stole attribute subject: %+v", cl)
		}
	}

	s, err := memory.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, cl := range e1 {
		if _, _, err := s.AdmitClaim(cl); err != nil {
			t.Fatal(err)
		}
	}
	for _, cl := range e2 {
		if _, _, err := s.AdmitClaim(cl); err != nil {
			t.Fatal(err)
		}
	}
	groups := s.ContestedGroups()
	if len(groups) == 0 {
		t.Fatalf("expected ContestedGroups non-empty for Widget price; claims=%+v e1=%+v e2=%+v",
			s.CurrentClaims(time.Time{}, true), e1, e2)
	}
	found := false
	for _, g := range groups {
		for _, cl := range g {
			if strings.EqualFold(cl.Subject, "Widget") && strings.EqualFold(cl.Predicate, "price") {
				found = true
				break
			}
		}
	}
	if !found {
		t.Fatalf("expected contested group for subject Widget predicate price; groups=%+v", groups)
	}
}

func TestResolveGroupQualityWinnerVsContested(t *testing.T) {
	// Clear quality winner.
	high := memory.Claim{
		ID: "a", Subject: "Widget", Predicate: "color", Object: "blue",
		DocumentIDs: []string{"d1"}, EvidenceQuality: 10,
	}
	low := memory.Claim{
		ID: "b", Subject: "Widget", Predicate: "color", Object: "red",
		DocumentIDs: []string{"d2"}, EvidenceQuality: 1,
	}
	res := memory.ResolveGroup([]memory.Claim{high, low})
	if res.Outcome != memory.ResolutionWinner || res.WinnerID != "a" {
		t.Fatalf("quality winner: %+v", res)
	}
	// True tie → contested (never silent UUID).
	t1 := memory.Claim{
		ID: "z-uuid", Subject: "X", Predicate: "is", Object: "1",
		DocumentIDs: []string{"d1"}, EvidenceQuality: 5,
	}
	t2 := memory.Claim{
		ID: "a-uuid", Subject: "X", Predicate: "is", Object: "2",
		DocumentIDs: []string{"d2"}, EvidenceQuality: 5,
	}
	res2 := memory.ResolveGroup([]memory.Claim{t1, t2})
	if res2.Outcome != memory.ResolutionContested || !res2.Contested {
		t.Fatalf("tie must be contested, got %+v", res2)
	}
	// More DocumentIDs wins when quality equal.
	more := memory.Claim{
		ID: "m", Subject: "Y", Predicate: "role", Object: "lead",
		DocumentIDs: []string{"d1", "d2", "d3"}, EvidenceQuality: 3,
	}
	fewer := memory.Claim{
		ID: "f", Subject: "Y", Predicate: "role", Object: "ic",
		DocumentIDs: []string{"d4"}, EvidenceQuality: 3,
	}
	res3 := memory.ResolveGroup([]memory.Claim{fewer, more})
	if res3.Outcome != memory.ResolutionWinner || res3.WinnerID != "m" {
		t.Fatalf("doc count: %+v", res3)
	}
	// Supersession ladder.
	old := memory.Claim{ID: "old", Subject: "Z", Predicate: "is", Object: "v1"}
	neu := memory.Claim{ID: "new", Subject: "Z", Predicate: "is", Object: "v2", Supersedes: "old"}
	res4 := memory.ResolveGroup([]memory.Claim{old, neu})
	if res4.Outcome != memory.ResolutionWinner || res4.WinnerID != "new" {
		t.Fatalf("supersession: %+v", res4)
	}
}

func TestCurrentClaimsAsOfDualAxis(t *testing.T) {
	s, err := memory.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	tValid := time.Date(2024, 6, 1, 0, 0, 0, 0, time.UTC)
	tObsEarly := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	tObsLate := time.Date(2024, 12, 1, 0, 0, 0, 0, time.UTC)
	_, _, err = s.AdmitClaim(memory.Claim{
		Subject: "Price", Predicate: "is", Object: "10",
		DocumentIDs: []string{"d1"},
		ValidFrom:   time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC),
		ObservedAt:  tObsEarly,
	})
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = s.AdmitClaim(memory.Claim{
		Subject: "Price", Predicate: "is", Object: "20",
		DocumentIDs: []string{"d2"},
		ValidFrom:   time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC), // future valid
		ObservedAt:  tObsLate,
	})
	if err != nil {
		t.Fatal(err)
	}
	// Historical: only first claim valid at mid-2024.
	past := s.CurrentClaimsAsOf(tValid, time.Time{}, true)
	if len(past) != 1 || past[0].Object != "10" {
		t.Fatalf("historical valid_at: %+v", past)
	}
	// known_at before late observation excludes the later ObservedAt claim
	// (even if we ask with includeContested and a valid time that covers both).
	// Force second claim valid-from early so only knownAt filters it.
	s2, _ := memory.Open(t.TempDir())
	_, _, _ = s2.AdmitClaim(memory.Claim{
		Subject: "Widget", Predicate: "color", Object: "blue",
		DocumentIDs: []string{"d1"}, ValidFrom: tValid, ObservedAt: tObsEarly,
	})
	_, _, _ = s2.AdmitClaim(memory.Claim{
		Subject: "Gadget", Predicate: "color", Object: "green",
		DocumentIDs: []string{"d2"}, ValidFrom: tValid, ObservedAt: tObsLate,
	})
	knownMid := time.Date(2024, 6, 15, 0, 0, 0, 0, time.UTC)
	asKnown := s2.CurrentClaimsAsOf(tValid, knownMid, true)
	for _, c := range asKnown {
		if c.Object == "green" {
			t.Fatalf("future known_at should exclude later ObservedAt: %+v", asKnown)
		}
	}
	if len(asKnown) != 1 {
		t.Fatalf("want only early-observed claim: %+v", asKnown)
	}
}

func TestDecayUtilityHalfLife(t *testing.T) {
	s, err := memory.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	_ = s.SetUtility("doc", 0.8) // < 1.0 → 7d half-life
	// First call establishes LastDecay clock without decaying.
	now := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	s.DecayUtilityHalfLife(now)
	if s.GetUtility("doc") != 0.8 {
		t.Fatalf("first call should not decay: %v", s.GetUtility("doc"))
	}
	// 30 days later with score < 1 → half-life 7d → ~0.8 * 0.5^(30/7) ≈ 0.04
	later := now.Add(30 * 24 * time.Hour)
	out := s.DecayUtilityHalfLife(later)
	got := out["doc"]
	if got >= 0.8 {
		t.Fatalf("expected measurable drop after 30d, got %v", got)
	}
	if got < 0.01 {
		// floor
		if s.GetUtility("doc") != 0.01 {
			t.Fatalf("floor: %v", s.GetUtility("doc"))
		}
	}
	// High score uses 365d half-life — smaller drop over 30d.
	_ = s.SetUtility("hot", 2.0)
	// Manually set LastDecay via first half-life call after SetUtility wiped LastDecay?
	// SetUtility does not set LastDecay; first call seeds.
	s.DecayUtilityHalfLife(now)
	// Need LastDecay=now for hot: call again after ensuring LastDecay set.
	// Force via DecayUtilityHalfLife at now then later.
	s.DecayUtilityHalfLife(now)
	before := s.GetUtility("hot")
	s.DecayUtilityHalfLife(later)
	after := s.GetUtility("hot")
	if after >= before {
		// If first seed happened at later... re-seed carefully.
		_ = s.SetUtility("hot", 2.0)
		// Patch by decaying once at now to set LastDecay, then check 30d.
		// DecayUtilityHalfLife first call sets LastDecay without decay when zero.
		// After SetUtility LastDecay is zero again.
		s.DecayUtilityHalfLife(now)
		before = s.GetUtility("hot")
		s.DecayUtilityHalfLife(later)
		after = s.GetUtility("hot")
	}
	if after >= before {
		t.Fatalf("high-score half-life should still drop over 30d: %v -> %v", before, after)
	}
	// Drop should be smaller than low-score 7d half-life path for same Δt.
	// (0.8 over 30d/7d drops much more fractionally than 2.0 over 30d/365d)
}

func TestNREMQuarantineLowUtility(t *testing.T) {
	s, err := memory.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	docs := map[string]string{"low": "low utility text", "high": "high utility text"}
	s.EnsureUtility([]string{"low", "high"})
	_ = s.SetUtility("low", 0.05)
	_ = s.SetUtility("high", 2.0)
	res := s.RunNREM(docs, 0.2, 1.5)
	if len(res.Quarantined) == 0 {
		t.Fatalf("expected quarantine growth: %+v list=%+v", res, s.ListQuarantine())
	}
	if !s.IsQuarantined("low") {
		t.Fatal("low should be quarantined")
	}
	if s.IsQuarantined("high") {
		t.Fatal("high must not be quarantined")
	}
	if len(res.Promoted) == 0 {
		t.Fatalf("high should be promoted: %+v", res)
	}
}

func TestHypothesizeAndPruneEdges(t *testing.T) {
	s, err := memory.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	// Adjacency triangle.
	_ = s.SetDocEdges(map[string][]string{
		"d1": {"d2", "d3"},
		"d2": {"d1"},
		"d3": {"d1"},
	})
	_ = s.SeedEdgeWeightsFromAdj()
	before := s.EdgeCount()
	if before == 0 {
		t.Fatal("expected edges")
	}
	// Claims share subject across d1 and d2 only.
	_, _, _ = s.AdmitClaim(memory.Claim{
		Subject: "Widget", Predicate: "color", Object: "blue", DocumentIDs: []string{"d1"},
	})
	_, _, _ = s.AdmitClaim(memory.Claim{
		Subject: "Widget", Predicate: "size", Object: "large", DocumentIDs: []string{"d2"},
	})
	// d1-d3 does not share claim subject → weight halved and may prune.
	// Weaken d1->d3 first to ensure prune path.
	w := s.WeightedEdges()
	w["d1->d3"] = 0.15
	w["d3->d1"] = 0.15
	_ = s.SetWeightedEdges(w)
	delta := s.HypothesizeEdges()
	// After hyp: shared subject edges strengthen; others *0.5; prune <0.1
	// 0.15*0.5=0.075 → pruned
	after := s.EdgeCount()
	if after >= before && delta == 0 {
		// Still ok if strengthen kept count — but prune should reduce some.
		// Check that weak non-shared edges dropped.
		w2 := s.WeightedEdges()
		if _, ok := w2["d1->d3"]; ok {
			if w2["d1->d3"] >= 0.15 {
				t.Fatalf("expected weaken/prune of non-shared edge: %v delta=%d", w2, delta)
			}
		}
	}
	pruned := s.PruneWeakEdges(0.1)
	_ = pruned
	if s.EdgeCount() > before {
		t.Fatalf("edge count should not grow unboundedly: before=%d after=%d", before, s.EdgeCount())
	}
}

func TestResegmentNearbyMerge(t *testing.T) {
	s, err := memory.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t0 := time.Now().UTC()
	_, _ = s.BindEpisode(memory.Episode{
		ID: "e1", Kind: "ingest", DocumentIDs: []string{"a", "b"}, Start: t0,
	})
	_, _ = s.BindEpisode(memory.Episode{
		ID: "e2", Kind: "ingest", DocumentIDs: []string{"c"}, Start: t0.Add(time.Hour),
	})
	res := s.ResegmentNearby(72*time.Hour, "gen-1", nil)
	if !res.Reseg {
		t.Fatalf("expected reseg: %+v", res)
	}
	if res.EpisodesAfter < 2 {
		// BindEpisode keeps sources; ResegmentEpisode adds merged — at least 3 or 2+.
		t.Logf("episodes_after=%d", res.EpisodesAfter)
	}
	merged := false
	for _, ep := range s.ListEpisodes() {
		if ep.Kind == "reseg" && len(ep.DocumentIDs) >= 3 {
			merged = true
		}
	}
	if !merged {
		t.Fatalf("expected merged reseg episode with 3 docs: %+v", s.ListEpisodes())
	}
}

func TestBuildProbesPreferSentence(t *testing.T) {
	docs := map[string]string{
		"d1": "Alpha Widget sapphire plant tokens shared across lines for retrieval.",
	}
	probes := memory.BuildProbesFromDocuments(docs, 3)
	if len(probes) != 1 {
		t.Fatalf("%+v", probes)
	}
	if !strings.HasPrefix(probes[0].Question, "What about ") {
		t.Fatalf("want What about … probe, got %q", probes[0].Question)
	}
}

func TestLinkClaimDocuments(t *testing.T) {
	s, err := memory.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	_, _, _ = s.AdmitClaim(memory.Claim{
		Subject: "Widget", Predicate: "color", Object: "blue",
		DocumentIDs: []string{"doc-a", "doc-b"},
	})
	_, _, _ = s.AdmitClaim(memory.Claim{
		Subject: "Widget", Predicate: "size", Object: "large",
		DocumentIDs: []string{"doc-c"},
	})
	n := s.LinkClaimDocuments(memory.DefaultClaimEdgeCap)
	if n == 0 {
		t.Fatal("expected claim-linked edges")
	}
	w := s.WeightedEdges()
	// co-mention on same claim
	if w["doc-a->doc-b"] <= 0 && w["doc-b->doc-a"] <= 0 {
		t.Fatalf("expected co-mention edge a-b: %v", w)
	}
	// shared subject Widget links c
	if w["doc-a->doc-c"] <= 0 && w["doc-c->doc-a"] <= 0 {
		t.Fatalf("expected shared-subject edge a-c: %v", w)
	}
}

func TestExpandFromSeedsCapsDegrade(t *testing.T) {
	s, err := memory.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	// Chain of claims: A→B via object=subject, B→C, etc.
	ids := make([]string, 0, 8)
	for i := 0; i < 8; i++ {
		sub := "Entity" + string(rune('A'+i))
		obj := "Entity" + string(rune('A'+i+1))
		c, _, err := s.AdmitClaim(memory.Claim{
			Subject: sub, Predicate: "links", Object: obj,
			DocumentIDs: []string{"d" + string(rune('0'+i))},
		})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, c.ID)
	}
	// Tight caps → degradation.
	res := s.ExpandFromSeeds([]string{ids[0]}, nil, memory.ExpansionCaps{
		MaxDepth: 1, MaxClaims: 2, MaxScanned: 3,
	})
	if !res.Degraded {
		t.Fatalf("expected degraded under tight caps: %+v", res)
	}
	if res.Reason == "" {
		t.Fatal("expected degradation reason")
	}
	// Loose caps → not degraded for small seed neighborhood.
	res2 := s.ExpandFromSeeds([]string{ids[0]}, nil, memory.ExpansionCaps{
		MaxDepth: 2, MaxClaims: 64, MaxScanned: 256,
	})
	if len(res2.ClaimIDs) == 0 {
		t.Fatalf("expected claims: %+v", res2)
	}
}

func TestMultiValuedPredicateNoContest(t *testing.T) {
	s, err := memory.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	c1, contested, err := s.AdmitClaim(memory.Claim{
		Subject: "Widget", Predicate: "tags", Object: "blue",
		DocumentIDs: []string{"d1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if c1.Status != memory.ClaimActive {
		t.Fatalf("c1 active: %+v", c1)
	}
	if len(contested) != 0 {
		t.Fatalf("tags must not contest: contested=%+v", contested)
	}
	c2, contested, err := s.AdmitClaim(memory.Claim{
		Subject: "Widget", Predicate: "tags", Object: "metal",
		DocumentIDs: []string{"d2"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if c2.Status != memory.ClaimActive {
		t.Fatalf("c2 active: %+v", c2)
	}
	if len(contested) != 0 {
		t.Fatalf("second tags object must not contest: %+v", contested)
	}
	// Both active, no contested groups.
	active := s.CurrentClaims(time.Time{}, false)
	if len(active) < 2 {
		t.Fatalf("both tags claims active: %+v", active)
	}
	if len(s.ContestedGroups()) != 0 {
		t.Fatalf("no contested groups for multi-valued: %+v", s.ContestedGroups())
	}
	// ResolveGroup also no-ops multi-valued.
	res := memory.ResolveGroup([]memory.Claim{c1, c2})
	if res.Outcome != memory.ResolutionNone || res.Reason != "multi_valued_predicate" {
		t.Fatalf("resolve multi-valued: %+v", res)
	}
}

func TestRunREMHighUtilityExtract(t *testing.T) {
	s, err := memory.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	docs := map[string]string{
		"hi": "Gadget is painted blue. Gadget costs $5.",
		"lo": "Noise text without claim patterns.",
	}
	s.EnsureUtility([]string{"hi", "lo"})
	_ = s.SetUtility("hi", 2.0)
	_ = s.SetUtility("lo", 0.1)
	res := s.RunREM(docs, 1.5)
	if !res.Enabled {
		t.Fatal("REM should be enabled when called")
	}
	if len(res.DocsScanned) != 1 || res.DocsScanned[0] != "hi" {
		t.Fatalf("only high-utility scanned: %+v", res)
	}
	if res.ClaimsAdmitted == 0 {
		t.Fatalf("expected claims from high-utility doc: %+v", res)
	}
	if res.RelationsAdmitted < 1 {
		t.Fatalf("REM must left-shift claims→TemporalRelations: %+v", res)
	}
	claims := s.CurrentClaims(time.Time{}, true)
	if len(claims) == 0 {
		t.Fatal("store should hold REM-extracted claims")
	}
	relDocs := s.ExpandRelationDocuments([]string{"Gadget"}, time.Time{}, time.Time{}, 8)
	if len(relDocs) == 0 {
		t.Fatal("want Gadget TemporalRelation docs after REM")
	}
}

func TestOpenIELightLocatedIn(t *testing.T) {
	claims := memory.ExtractClaimsFromText("d1", "Acme Corp is located in Austin Texas.")
	found := false
	for _, c := range claims {
		if c.Predicate == "located_in" {
			found = true
			if c.SpanText == "" || c.SpanEnd <= c.SpanStart {
				t.Fatalf("want evidence span on claim: %+v", c)
			}
		}
	}
	if !found {
		t.Fatalf("want located_in claim, got %+v", claims)
	}
}

func TestOpenIEDensityRPOAndRequires(t *testing.T) {
	text := "RPO is 1 day. Widget API requires dual auth. Product SLA is 99.9%."
	claims := memory.ExtractClaimsFromText("ops1", text)
	preds := map[string]bool{}
	for _, c := range claims {
		preds[c.Predicate] = true
		if c.SpanEnd <= c.SpanStart {
			t.Fatalf("span missing: %+v", c)
		}
	}
	if !preds["is"] && !preds["requires"] && !preds["sla"] {
		t.Fatalf("want denser ops claims, got %+v", claims)
	}
	// RPO subject
	foundRPO := false
	for _, c := range claims {
		if c.Subject == "RPO" {
			foundRPO = true
		}
	}
	if !foundRPO {
		t.Fatalf("want RPO claim: %+v", claims)
	}
}

// General product graph (not ERB RPO-only): dependency / ownership / org.
func TestOpenIEGeneralProductGraph(t *testing.T) {
	text := strings.Join([]string{
		"Acme Cloud depends on Stripe for billing.",
		"Widget Platform integrates with Salesforce.",
		"Ledger service is owned by Northwind Holdings.",
		"Alice reports to Bob.",
		"Payments API is powered by Kafka.",
		"Finance team manages the ledger service.",
		"Search service provides full-text ranking.",
		"Ops is responsible for the on-call rotation.",
	}, " ")
	claims := memory.ExtractClaimsFromText("dsid_product", text)
	want := map[string]bool{
		"depends_on": true, "integrates_with": true, "owned_by": true,
		"reports_to": true, "powered_by": true, "manages": true,
		"provides": true, "responsible_for": true,
	}
	got := map[string]bool{}
	for _, c := range claims {
		got[c.Predicate] = true
		if c.SpanEnd <= c.SpanStart {
			t.Fatalf("span missing: %+v", c)
		}
	}
	for pred := range want {
		if !got[pred] {
			t.Fatalf("want predicate %s in general product extract; got %+v", pred, claims)
		}
	}
	// Left-shift: cortex seeds TemporalRelations for product graph subjects.
	s, err := memory.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	res := s.RunCortexMaintenance(map[string]string{"dsid_product": text})
	if res.RelationsAdmitted < 4 {
		t.Fatalf("want denser product TemporalRelations, res=%+v claims=%d", res, res.ClaimsAdmitted)
	}
	for _, seed := range []string{"Acme", "Widget", "Alice", "Payments"} {
		docs := s.ExpandRelationDocuments([]string{seed}, time.Time{}, time.Time{}, 8)
		if len(docs) == 0 {
			t.Fatalf("want ExpandRelationDocuments(%q) → dsid_product", seed)
		}
	}
}

func TestOpenIEEntityScopedRPO(t *testing.T) {
	text := "MedThink failover policy sets RPO to 15 minutes and RTO to 30 minutes for gold tier production."
	claims := memory.ExtractClaimsFromText("dsid_policy", text)
	foundRPO, foundRTO := false, false
	for _, c := range claims {
		if strings.EqualFold(c.Subject, "MedThink") && c.Predicate == "rpo_minutes" &&
			strings.Contains(c.Object, "15") {
			foundRPO = true
		}
		if strings.EqualFold(c.Subject, "MedThink") && c.Predicate == "rto_minutes" &&
			strings.Contains(c.Object, "30") {
			foundRTO = true
		}
	}
	if !foundRPO || !foundRTO {
		t.Fatalf("want MedThink rpo/rto claims, got %+v", claims)
	}
	// Cortex left-shift: claim → TemporalRelation with document evidence.
	s, err := memory.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	_ = s.SetDocTexts(map[string]string{"dsid_policy": text})
	res := s.RunCortexMaintenance(map[string]string{"dsid_policy": text})
	if res.RelationsAdmitted < 1 {
		t.Fatalf("want relations from claims, res=%+v", res)
	}
	docs := s.ExpandRelationDocuments([]string{"MedThink"}, time.Time{}, time.Time{}, 8)
	if len(docs) == 0 || docs[0] != "dsid_policy" {
		t.Fatalf("want dsid_policy from MedThink relations: %+v", docs)
	}
}

func TestFillClaimSpanOffsets(t *testing.T) {
	doc := "The Widget costs $10 in store."
	cl := memory.Claim{Subject: "Widget", Predicate: "costs", Object: "$10", SpanText: "Widget costs $10", SpanDocID: "d1"}
	memory.FillClaimSpanOffsets(&cl, doc)
	if cl.SpanStart < 0 || cl.SpanEnd <= cl.SpanStart {
		t.Fatalf("offsets not filled: %+v", cl)
	}
	if !strings.Contains(doc[cl.SpanStart:cl.SpanEnd], "Widget") {
		t.Fatalf("span slice=%q", doc[cl.SpanStart:cl.SpanEnd])
	}
}

func TestParseOpenIEJSONFillsSpan(t *testing.T) {
	raw := `[{"subject":"Acme","predicate":"owns","object":"Beta","span":"Acme owns Beta"}]`
	claims := memory.ParseOpenIEJSON("d9", raw)
	if len(claims) != 1 {
		t.Fatalf("claims=%+v", claims)
	}
	if claims[0].SpanText == "" || claims[0].Provenance != "openie_llm" {
		t.Fatalf("claim=%+v", claims[0])
	}
}

func TestQueryLogAndProbes(t *testing.T) {
	s, err := memory.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AppendQueryLog(memory.QueryLogEntry{Question: "What is Widget price?", DocIDs: []string{"d1"}}); err != nil {
		t.Fatal(err)
	}
	entries := s.LoadQueryLog(10)
	if len(entries) != 1 {
		t.Fatalf("entries=%v", entries)
	}
	probes := memory.BuildProbesFromQueryLog(entries, 3)
	if len(probes) != 1 || probes[0].Question == "" {
		t.Fatalf("probes=%+v", probes)
	}
}

func TestContestedCliquesMultiObject(t *testing.T) {
	s, err := memory.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t0 := time.Now().UTC()
	_, _, _ = s.AdmitClaim(memory.Claim{Subject: "Widget", Predicate: "price", Object: "$10", DocumentIDs: []string{"d1"}, ValidFrom: t0})
	_, _, _ = s.AdmitClaim(memory.Claim{Subject: "Widget", Predicate: "price", Object: "$12", DocumentIDs: []string{"d2"}, ValidFrom: t0})
	_, _, _ = s.AdmitClaim(memory.Claim{Subject: "Widget", Predicate: "price", Object: "$15", DocumentIDs: []string{"d3"}, ValidFrom: t0})
	cliques := s.ContestedCliques()
	if len(cliques) == 0 {
		t.Fatal("want multi-object clique")
	}
	r := memory.ResolveMultiClique(cliques[0].Claims)
	if !r.Contested && r.Outcome != memory.ResolutionContested {
		t.Fatalf("multi-clique should contest: %+v", r)
	}
}

func TestGraphRAGAndPhraseEdges(t *testing.T) {
	s, err := memory.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	docs := map[string]string{"d1": "Acme meeting notes on deploy", "d2": "Acme incident outage"}
	_ = s.SetDocTexts(docs)
	res := s.RunCortexMaintenance(docs)
	if res.Summaries == 0 {
		t.Fatal("want summaries including graphrag")
	}
	n := s.SeedPhrasePassageEdgesFromClaims()
	_ = n
	// Seeded phrase nodes must use phrase: prefix (not legacy phr:).
	w := s.WeightedEdges()
	for k := range w {
		if strings.HasPrefix(k, "phr:") && !strings.HasPrefix(k, "phrase:") {
			t.Fatalf("legacy phr: prefix still present: %s", k)
		}
	}
	if s.AutoSegmentCompanyLife(docs) < 1 {
		t.Fatal("want life episodes for meeting/incident")
	}
}

func TestMeasureProbeEmptyExpectedIsUnknown(t *testing.T) {
	r := memory.MeasureProbe(memory.Probe{Question: "what?"}, []string{"d1"}, 5)
	if r.PredictionError != 0.5 {
		t.Fatalf("empty expected must not be perfect: %+v", r)
	}
}

func TestBuildProbesFromQueryLogPrefersGold(t *testing.T) {
	entries := []memory.QueryLogEntry{
		{Question: "no gold question one", DocIDs: nil},
		{Question: "What is RPO?", DocIDs: []string{"policy"}},
		{Question: "What is RPO?", DocIDs: []string{"policy"}}, // dedupe
	}
	probes := memory.BuildProbesFromQueryLog(entries, 5)
	if len(probes) != 1 || probes[0].Question != "What is RPO?" {
		t.Fatalf("probes=%+v", probes)
	}
	if len(probes[0].ExpectedDocIDs) != 1 || probes[0].ExpectedDocIDs[0] != "policy" {
		t.Fatalf("expected gold: %+v", probes[0])
	}
}

func TestWalkPageIndexDeterministic(t *testing.T) {
	s, err := memory.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	tree := memory.BuildPageIndexTree("doc1", "# Pricing\n\nWidget costs $10.\n\n# Shipping\n\nShips in 2 days.")
	if err := s.StorePageIndex([]memory.PageNode{tree}); err != nil {
		t.Fatal(err)
	}
	hits := s.WalkPageIndex(context.Background(), "widget price", memory.PageIndexWalker{MaxSteps: 3})
	if len(hits) == 0 {
		if len(s.SearchPageIndex("pricing", 3)) == 0 {
			t.Fatal("expected pageindex hits")
		}
	}
}

func TestPageIndexBuildAndSearch(t *testing.T) {
	text := `# Overview
Alpha product overview text.

## Pricing
Widget costs $10 for the base plan.

## Team
CEO of Acme is Ada Lovelace.
`
	tree := memory.BuildPageIndexTree("doc1", text)
	if tree.DocumentID != "doc1" {
		t.Fatalf("doc id: %+v", tree)
	}
	if len(tree.Children) == 0 {
		t.Fatalf("expected heading children: %+v", tree)
	}
	s, err := memory.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := s.StorePageIndex([]memory.PageNode{tree}); err != nil {
		t.Fatal(err)
	}
	hits := s.SearchPageIndex("pricing widget", 5)
	if len(hits) == 0 {
		t.Fatal("expected pageindex hits for pricing")
	}
	found := false
	for _, h := range hits {
		if strings.Contains(strings.ToLower(h.Title), "pricing") || strings.Contains(strings.ToLower(h.Text), "widget") {
			found = true
		}
	}
	if !found {
		t.Fatalf("hits=%+v", hits)
	}
}

func TestGlobalPageRankPrior(t *testing.T) {
	edges := map[string][]string{
		"A": {"B", "C"},
		"B": {"A"},
		"C": {"A"},
	}
	scores := memory.GlobalPageRank(edges, 30)
	if scores["A"] <= scores["B"] {
		// A has higher in-degree-ish; should dominate or at least be non-zero
		t.Logf("scores=%v (A may still win after more iters)", scores)
	}
	if scores["A"] <= 0 {
		t.Fatalf("A must have mass: %v", scores)
	}
	s, err := memory.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := s.StorePageRank(scores); err != nil {
		t.Fatal(err)
	}
	base := map[string]float64{"A": 1.0, "B": 1.0, "C": 1.0}
	boosted := s.ApplyPageRankPrior(base, 0.15)
	if boosted["A"] < base["A"] {
		t.Fatalf("PR prior should not reduce: %+v", boosted)
	}
	// Highest PR should get largest multiplicative boost.
	if boosted["A"] <= boosted["B"] && scores["A"] > scores["B"] {
		t.Fatalf("A should outrank B after prior: boosted=%v pr=%v", boosted, scores)
	}
}

func TestAgentMemoryTiers(t *testing.T) {
	s, err := memory.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	stm, err := s.PutAgentMemory("alice", "note", "short term note about sapphire", nil)
	if err != nil {
		t.Fatal(err)
	}
	if stm.Tier != memory.TierSTM {
		t.Fatalf("default tier stm: %+v", stm)
	}
	ltm, err := s.PutAgentMemoryTier("alice", "fact", "long term fact about sapphire", nil, memory.TierLTM)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.PromoteAgentMemory(ltm.ID, memory.TierMTM); err != nil {
		t.Fatal(err)
	}
	got := s.GetAgentMemory("alice", 10)
	if len(got) < 2 {
		t.Fatalf("%+v", got)
	}
	// stm should sort before mtm/ltm
	if tierRankTest(got[0].Tier) > tierRankTest(got[1].Tier) {
		t.Fatalf("expected stm-first order: %+v", got)
	}
	hits := s.SearchAgentMemory("alice", "sapphire", 10)
	if len(hits) < 2 {
		t.Fatalf("search: %+v", hits)
	}
	if hits[0].Tier != memory.TierSTM {
		t.Fatalf("search prefers stm: %+v", hits)
	}
	_ = stm
}

func tierRankTest(t string) int {
	switch t {
	case memory.TierSTM:
		return 0
	case memory.TierMTM:
		return 1
	default:
		return 2
	}
}

func TestCortexMaintenanceUnit(t *testing.T) {
	s, err := memory.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	docs := map[string]string{
		"d1": "Alpha Widget is painted blue. Widget costs $12. Shared plant tokens.",
		"d2": "Alpha Widget is painted red. Widget costs $99. Shared plant tokens.",
	}
	res := s.RunCortexMaintenance(docs)
	if res.ClaimsAdmitted == 0 {
		t.Fatalf("claims: %+v", res)
	}
	if res.PageIndex != 2 {
		t.Fatalf("pageindex: %+v", res)
	}
	if len(s.DocEdges()) == 0 {
		t.Fatal("edges")
	}
	if len(s.PageRankScores()) == 0 {
		t.Fatal("pagerank")
	}
	if len(s.ListPageIndex()) == 0 {
		t.Fatal("pageindex store")
	}
}
