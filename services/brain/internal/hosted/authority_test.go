package hosted

import "testing"

func TestApplyAuthorityRecencyConflictPrefersRecent(t *testing.T) {
	pool := []Passage{
		{DocumentID: "slack-old", Text: "Policy says RPO is 24h as of 2019-01-01", Score: 0.9, Channel: "slack"},
		{DocumentID: "confluence-new", Text: "Superseding policy RPO is 4h effective 2026-03-15", Score: 0.5, Channel: "confluence"},
	}
	out, diag := applyAuthorityRecency(pool, "conflicting_info", nil)
	if diag["authority_rank"] != "ok" {
		t.Fatalf("diag=%v", diag)
	}
	if len(out) != 2 {
		t.Fatalf("len=%d", len(out))
	}
	if out[0].DocumentID != "confluence-new" {
		t.Fatalf("want confluence-new first, got %s (diag=%v)", out[0].DocumentID, diag)
	}
	if diag["top_date"] != "2026-03-15" {
		t.Fatalf("want exact top_date 2026-03-15, diag=%v", diag)
	}
}

func TestApplyAuthorityRecencyAlwaysOnBasicExactDates(t *testing.T) {
	// Leave-policy: 2026-05-10 (5 days) vs 2026-06-01 (10 days).
	// Must use exact calendar order — not decade-scale flattening.
	pool := []Passage{
		{
			DocumentID: "dsid_old5",
			Text:       "Under Redwood's current policy effective 2026-05-10, bereavement leave provides up to 5 days paid for immediate family.",
			Score:      0.95,
			Channel:    "confluence",
		},
		{
			DocumentID: "dsid_new10",
			Text:       "Compassionate Loss Support policy supersedes prior bereavement leave. Effective 2026-06-01, Redwood provides up to 10 days paid bereavement leave for immediate family. Current policy.",
			Score:      0.55,
			Channel:    "confluence",
		},
	}
	out, diag := applyAuthorityRecencyQ(
		pool,
		"How many days of bereavement leave does current policy provide?",
		"basic",
		nil,
	)
	if diag["authority_rank"] != "ok" {
		t.Fatalf("diag=%v", diag)
	}
	if out[0].DocumentID != "dsid_new10" {
		t.Fatalf("want newest superseding policy first, got %s (diag=%v)", out[0].DocumentID, diag)
	}
	if diag["top_date"] != "2026-06-01" {
		t.Fatalf("want exact date 2026-06-01 preserved in diag, got %v", diag["top_date"])
	}
	if diag["authority_mode"] != "date_primary_conflict" {
		t.Fatalf("leave/current policy should use date_primary_conflict, diag=%v", diag)
	}
}

func TestPassageTimeKeepsDayResolution(t *testing.T) {
	may, ok1 := passageTime(Passage{Text: "effective 2026-05-10 five days"})
	june, ok2 := passageTime(Passage{Text: "effective 2026-06-01 ten days supersedes"})
	if !ok1 || !ok2 {
		t.Fatalf("parse failed may=%v june=%v", ok1, ok2)
	}
	if !june.After(may) {
		t.Fatalf("june must be after may: may=%s june=%s", may, june)
	}
	// One-day gap must remain distinguishable (not decade squash).
	sep1, _ := passageTime(Passage{Text: "updated 2026-09-01"})
	sep2, _ := passageTime(Passage{Text: "updated 2026-09-02"})
	if !sep2.After(sep1) || sep2.Sub(sep1).Hours() != 24 {
		t.Fatalf("day resolution lost: %s vs %s", sep1, sep2)
	}
}

func TestApplyAuthorityRecencyPreservesTurnFront(t *testing.T) {
	pool := []Passage{
		{DocumentID: "turn:s:1", Text: "prior chat", Score: 0.1, Channel: "turn_grep"},
		{DocumentID: "old", Text: "Policy 2019-01-01", Score: 0.9},
		{DocumentID: "new", Text: "Policy effective 2026-06-01 supersedes", Score: 0.4},
	}
	out, _ := applyAuthorityRecency(pool, "basic", nil)
	if out[0].DocumentID != "turn:s:1" {
		t.Fatalf("turn must stay front, got %s", out[0].DocumentID)
	}
}

func TestFactSlotChecklistCompleteness(t *testing.T) {
	slots := factSlotChecklist("What is the RPO and owner?", "completeness")
	if len(slots) < 2 {
		t.Fatalf("slots=%v", slots)
	}
}

func TestBestLast(t *testing.T) {
	in := []Passage{
		{DocumentID: "best", Text: "a"},
		{DocumentID: "mid", Text: "b"},
		{DocumentID: "worst", Text: "c"},
	}
	out := bestLast(in)
	if out[len(out)-1].DocumentID != "best" {
		t.Fatalf("best should be last: %+v", out)
	}
	withTurn := []Passage{
		{DocumentID: "turn:s:1", Text: "hi", Channel: "turn_grep"},
		{DocumentID: "best", Text: "a"},
		{DocumentID: "worst", Text: "c"},
	}
	out2 := bestLast(withTurn)
	if out2[0].DocumentID != "turn:s:1" {
		t.Fatalf("turn should stay front: %+v", out2)
	}
	if out2[len(out2)-1].DocumentID != "best" {
		t.Fatalf("best last among docs: %+v", out2)
	}
}
