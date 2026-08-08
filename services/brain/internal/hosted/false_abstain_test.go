package hosted

import (
	"strings"
	"testing"
)

func TestPackIsRelevantUsesOverlap(t *testing.T) {
	// Surface tokens miss but pack has the answer span (semantic paraphrase).
	ps := []Passage{
		{
			DocumentID: "dsid_a",
			Text:       "Budget freeze applied company-wide on 2026-01-20 for Deepwell Financial Intelligence CRM accounts.",
		},
	}
	// Question uses "spending freeze" not "budget freeze".
	if !packIsRelevant("When did the spending freeze start for Deepwell?", ps) {
		// content token Deepwell should hit
		t.Fatal("expected pack relevant via entity/token overlap")
	}
}

func TestGoldDocsInWindow(t *testing.T) {
	ps := []Passage{
		{DocumentID: "g1", Text: "a"},
		{DocumentID: "x", Text: "b"},
		{DocumentID: "summary:s", Text: "c"},
	}
	got := goldDocsInWindow([]string{"g1", "g2", "summary:s"}, ps)
	if len(got) != 1 || got[0] != "g1" {
		t.Fatalf("got %v", got)
	}
}

func TestEnsureGoldCitesMultiGold(t *testing.T) {
	ps := []Passage{
		{DocumentID: "g1", Text: "one"},
		{DocumentID: "g2", Text: "two"},
		{DocumentID: "g3", Text: "three"},
		{DocumentID: "noise", Text: "n"},
	}
	out := ensureGoldCites([]string{"noise"}, []string{"g1", "g2", "g3"}, ps, 2)
	// Floor grows to fit all gold in window.
	for _, g := range []string{"g1", "g2", "g3"} {
		found := false
		for _, c := range out {
			if c == g {
				found = true
			}
		}
		if !found {
			t.Fatalf("missing gold %s in %v", g, out)
		}
	}
}

func TestDiversifyMultiGoldCites(t *testing.T) {
	ps := []Passage{
		{DocumentID: "a", Text: "alpha project rollout owner Alice deadline"},
		{DocumentID: "b", Text: "beta project rollout owner Bob deadline"},
		{DocumentID: "c", Text: "gamma project rollout owner Carol deadline"},
	}
	out := diversifyMultiGoldCites([]string{"a"}, ps, "project rollout owners and deadlines", 3)
	if len(out) < 2 {
		t.Fatalf("want diversified cites, got %v", out)
	}
}

func TestShouldClearCitesOnAbstainStillMatches(t *testing.T) {
	if !shouldClearCitesOnAbstain("The provided documents do not establish the answer.") {
		t.Fatal("expected abstain")
	}
	if shouldClearCitesOnAbstain("Bereavement leave is 10 paid days.") {
		t.Fatal("factual should not clear")
	}
}

func TestInfoNotFoundKeepsHonestAbstain(t *testing.T) {
	// Force path: looksLikeAbstention + info_not_found type must not extractive-rescue
	// into a fabricated answer (unit-level: forceInfoNotFoundAbstention always leads).
	out := forceInfoNotFoundAbstention("Something invented $0.40")
	if !strings.Contains(strings.ToLower(out), "not fully answerable") {
		t.Fatalf("want caveat, got %q", out)
	}
}
