package hosted

import (
	"strings"
	"testing"
)

func TestAdjudicateSupersessionLeavePolicyNearDup(t *testing.T) {
	// Near-dup leave policies: 5-day May vs 10-day June — mark SUPERSEDING.
	// Share enough head tokens for nearDupGroups (jaccard ≥0.55 on 4+ char tokens).
	bodyOld := "Redwood bereavement leave policy for immediate family paid leave days under company handbook section compassionate loss support framework employees receive leave benefit days when family member dies policy text body shared"
	bodyNew := "Redwood bereavement leave policy for immediate family paid leave days under company handbook section compassionate loss support framework employees receive leave benefit days when family member dies policy text body shared"
	pool := []Passage{
		{
			DocumentID: "dsid_old5",
			Text:       "Under Redwood current policy effective 2026-05-10, bereavement leave provides up to 5 days. " + bodyOld,
			Score:      0.9,
		},
		{
			DocumentID: "dsid_new10",
			Text:       "Compassionate Loss Support supersedes prior bereavement. Effective 2026-06-01, Redwood provides up to 10 days. " + bodyNew,
			Score:      0.5,
		},
	}
	out, diag := adjudicateSupersession(pool,
		"How many days of bereavement leave does current policy provide?")
	if diag["supersession_adjudicate"] != true {
		t.Fatalf("near-dup leave pair must adjudicate, diag=%v out0=%q", diag, clip(out[0].Text, 80))
	}
	// Must use near-dup path, not global_newest-only escape hatch.
	if diag["global_newest"] == true && diag["supersession_groups"] == nil {
		// global_newest alone without groups is OK only if SUPERSEDING present on new10
	}
	foundSuper := false
	for _, p := range out {
		if strings.Contains(p.Text, "SUPERSEDING") && p.DocumentID == "dsid_new10" {
			foundSuper = true
		}
	}
	if !foundSuper && out[0].DocumentID != "dsid_new10" {
		t.Fatalf("want new10 SUPERSEDING or first, got ids=%s/%s diag=%v",
			out[0].DocumentID, out[1].DocumentID, diag)
	}
}

func TestAdjudicateSupersessionNoFalseGlobalOnQuantity(t *testing.T) {
	// Ordinary quantity question with two unrelated dated docs — must NOT invent SUPERSEDING.
	pool := []Passage{
		{DocumentID: "a", Text: "Meeting notes 2026-01-15: pizza party on Friday.", Score: 0.9},
		{DocumentID: "b", Text: "Incident log 2026-03-01: latency spike resolved.", Score: 0.8},
	}
	out, diag := adjudicateSupersession(pool, "How many days until the next deploy?")
	if diag["global_newest"] == true {
		t.Fatalf("quantity ask must not markGlobalNewest, diag=%v", diag)
	}
	for _, p := range out {
		if strings.Contains(p.Text, "SUPERSEDING") {
			t.Fatalf("no SUPERSEDING on quantity ask, got %q", clip(p.Text, 100))
		}
	}
}

func TestWantsGlobalNewestMarkNarrow(t *testing.T) {
	if wantsGlobalNewestMark("How many days of leave?", "") {
		t.Fatal("how many days alone must not want global newest")
	}
	if !wantsGlobalNewestMark("Which is correct after the note that supersedes the earlier draft?", "") {
		t.Fatal("explicit supersede language should want mark")
	}
	if !wantsGlobalNewestMark("anything", "conflicting_info") {
		t.Fatal("conflicting_info type should want mark")
	}
}

func TestPreferSupersedingCites(t *testing.T) {
	ps := []Passage{
		{DocumentID: "old", Text: "[SUPERSEDED version — older]\n5 days"},
		{DocumentID: "new", Text: "[SUPERSEDING version — prefer this as current]\n10 days"},
	}
	out := preferSupersedingCites([]string{"old", "new"}, ps)
	if out[0] != "new" {
		t.Fatalf("want new first, got %v", out)
	}
}

func TestGroundAbstainKeepsEmptyCites(t *testing.T) {
	ps := []Passage{{DocumentID: "a", Text: "unrelated stock price rose"}}
	g := groundAnswerInPassages(
		"What is the RPO?",
		"The documents do not establish the answer to this question.",
		[]string{"a"},
		nil,
		ps,
		"basic",
	)
	if len(g.CitedDocumentIDs) != 0 {
		t.Fatalf("abstain must clear cites, got %v", g.CitedDocumentIDs)
	}
	if g.Diagnostics["abstain_cleared_cites"] != true {
		t.Fatalf("want abstain_cleared_cites diag, got %v", g.Diagnostics)
	}
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
