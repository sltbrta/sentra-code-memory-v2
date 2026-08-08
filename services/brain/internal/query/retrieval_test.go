package query

import (
	"context"
	"testing"
)

// TestSelectCandidatesPrefersExactPathMentions proves a query naming exact
// repository-relative paths selects exactly those files, with term matching
// scoped to the named files.
func TestSelectCandidatesPrefersExactPathMentions(t *testing.T) {
	corpus := buildFixtureCorpus(t)
	_, currentID := generationIDs(t, corpus)
	snapshot, err := corpus.Snapshot(context.Background(), testSourceID, currentID)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	candidates := selectCandidates(snapshot, tokenizeQuery(
		"Compare the anchor returned by src/go/modify-00.go with the function in src/typescript/modify-00.ts.",
	), DefaultLimits().MaxCandidates)
	if len(candidates) != 2 || candidates[0].path != "src/go/modify-00.go" ||
		candidates[1].path != "src/typescript/modify-00.ts" {
		t.Fatalf("candidates = %#v", candidates)
	}
}

// TestSelectCandidatesMatchesDefinitionTerms proves a path-free query selects
// files by exact definition spellings.
func TestSelectCandidatesMatchesDefinitionTerms(t *testing.T) {
	corpus := buildFixtureCorpus(t)
	_, currentID := generationIDs(t, corpus)
	snapshot, err := corpus.Snapshot(context.Background(), testSourceID, currentID)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	candidates := selectCandidates(snapshot, tokenizeQuery("What does anchor return?"), DefaultLimits().MaxCandidates)
	if len(candidates) == 0 {
		t.Fatal("definition term anchor must select files")
	}
	for _, candidate := range candidates {
		found := false
		for _, definition := range candidate.definitions {
			if definition == "anchor" || definition == "Anchor" {
				found = true
			}
		}
		if !found {
			t.Fatalf("candidate %q selected without an anchor definition: %#v", candidate.path, candidate)
		}
	}
}

// TestSelectCandidatesNeverMatchesInsideDegradedLanes proves lexically
// degraded files contribute no definition-term candidates.
func TestSelectCandidatesNeverMatchesInsideDegradedLanes(t *testing.T) {
	corpus := buildFixtureCorpus(t)
	_, currentID := generationIDs(t, corpus)
	snapshot, err := corpus.Snapshot(context.Background(), testSourceID, currentID)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	candidates := selectCandidates(snapshot, tokenizeQuery("Where is malformed defined?"), DefaultLimits().MaxCandidates)
	for _, candidate := range candidates {
		if candidate.path == "src/typescript/modify-00.ts" {
			t.Fatalf("degraded file must not be selected by term matching: %#v", candidate)
		}
	}
}

// TestSelectCandidatesIsBounded proves candidate selection enforces the
// configured cap deterministically in path order.
func TestSelectCandidatesIsBounded(t *testing.T) {
	corpus := buildFixtureCorpus(t)
	_, currentID := generationIDs(t, corpus)
	snapshot, err := corpus.Snapshot(context.Background(), testSourceID, currentID)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	candidates := selectCandidates(snapshot, tokenizeQuery("What does anchor return?"), DefaultLimits().MaxCandidates)
	if len(candidates) > DefaultLimits().MaxCandidates {
		t.Fatalf("candidates exceed the configured bound: %d", len(candidates))
	}
	for index := 1; index < len(candidates); index++ {
		if candidates[index-1].path > candidates[index].path {
			t.Fatalf("candidates out of path order: %#v", candidates)
		}
	}
}

// TestTokenizeQueryStripsPunctuation proves path tokens survive sentence
// punctuation while interior path characters are retained.
func TestTokenizeQueryStripsPunctuation(t *testing.T) {
	tokens := tokenizeQuery(`"Which function lives at src/go/rename-00.go?" she asked (really).`)
	assertContainsToken(t, tokens, "src/go/rename-00.go")
	assertContainsToken(t, tokens, "Which")
	assertContainsToken(t, tokens, "really")
}

func assertContainsToken(t *testing.T, tokens []string, want string) {
	t.Helper()
	for _, token := range tokens {
		if token == want {
			return
		}
	}
	t.Fatalf("tokens %v missing %q", tokens, want)
}

// TestEvaluateFreshness pins the three freshness requirements against
// current, superseded, and degraded pins.
func TestEvaluateFreshness(t *testing.T) {
	snapshot := Snapshot{GenerationID: "gen-1", Sequence: 1, State: GenerationReady}
	for _, scenario := range []struct {
		name        string
		requirement FreshnessRequirement
		currentID   string
		state       GenerationState
		wantState   FreshnessState
		wantStale   bool
		wantAbstain bool
	}{
		{"best_effort current", FreshnessBestEffort, "gen-1", GenerationReady, FreshnessCurrent, false, false},
		{"best_effort current degraded", FreshnessBestEffort, "gen-1", GenerationDegraded, FreshnessDegraded, false, false},
		{"best_effort stale", FreshnessBestEffort, "gen-2", GenerationReady, FreshnessStaleDisclosed, true, false},
		{"complete_generation stale", FreshnessCompleteGeneration, "gen-2", GenerationReady, FreshnessStaleDisclosed, true, false},
		{"abstain_if_stale stale", FreshnessAbstainIfStale, "gen-2", GenerationReady, FreshnessStaleDisclosed, true, true},
		{"abstain_if_stale current", FreshnessAbstainIfStale, "gen-1", GenerationReady, FreshnessCurrent, false, false},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			pinned := snapshot
			pinned.State = scenario.state
			evaluation := evaluateFreshness(scenario.requirement, pinned, scenario.currentID)
			if evaluation.State != scenario.wantState || evaluation.Stale != scenario.wantStale ||
				evaluation.AbstainStale != scenario.wantAbstain {
				t.Fatalf("evaluation = %#v, want state=%q stale=%t abstain=%t",
					evaluation, scenario.wantState, scenario.wantStale, scenario.wantAbstain)
			}
		})
	}
}

// TestComputeCoverageCountsIndexedWithinCanonical proves coverage counts
// revisions present in the projection against every canonical revision.
func TestComputeCoverage(t *testing.T) {
	corpus := buildFixtureCorpus(t)
	_, currentID := generationIDs(t, corpus)
	snapshot, err := corpus.Snapshot(context.Background(), testSourceID, currentID)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	coverage := computeCoverage(snapshot)
	if coverage.CanonicalRevisionCount != 75 || coverage.IndexedRevisionCount != 75 {
		t.Fatalf("coverage = %#v, want 75/75", coverage)
	}
	snapshot.Projection.State = ProjectionAbsent
	coverage = computeCoverage(snapshot)
	if coverage.CanonicalRevisionCount != 75 || coverage.IndexedRevisionCount != 0 {
		t.Fatalf("absent projection coverage = %#v, want 75/0", coverage)
	}
}
