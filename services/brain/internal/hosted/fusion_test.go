package hosted

import (
	"strings"
	"testing"
)

func TestRRFFuseManyAndRetain(t *testing.T) {
	a := []Hit{
		{ChunkID: "c1", DSID: "dsid_a", Text: "alpha foo bar", Score: 1, Channel: "lex"},
		{ChunkID: "c2", DSID: "dsid_b", Text: "beta", Score: 0.5, Channel: "lex"},
	}
	b := []Hit{
		{ChunkID: "c3", DSID: "dsid_b", Text: "beta ERROR-42 detail", Score: 0.9, Channel: "dense"},
		{ChunkID: "c1", DSID: "dsid_a", Text: "alpha foo bar", Score: 0.2, Channel: "dense"},
	}
	fused := rrfFuseMany([][]Hit{a, b}, 60)
	if len(fused) < 2 {
		t.Fatalf("fuse len %d", len(fused))
	}
	pool := hitsToPassages(fused, 10, 500)
	win, diag := progressiveRetainWindow(pool, "What about ERROR-42?", 3, 4, nil)
	if !diag["progressive"].(bool) {
		t.Fatalf("want progressive retain, diag=%v", diag)
	}
	if len(win) == 0 {
		t.Fatal("empty window")
	}
	// Identifier floor should keep ERROR-42 doc.
	found := false
	for _, p := range win {
		if p.DocumentID == "dsid_b" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected id floor dsid_b, got %+v diag=%v", win, diag)
	}
}

func TestProgressiveRetainKeepsGoldThroughShrink(t *testing.T) {
	// Many high-score noise docs + two low-score golds in the pool.
	// Final topK=4 must still keep both golds after wide→narrow.
	var pool []Passage
	for i := 0; i < 20; i++ {
		pool = append(pool, Passage{
			DocumentID: "noise_" + string(rune('a'+i%10)) + string(rune('0'+i/10)),
			Text:       "generic enterprise ops note without special ids",
			Score:      1.0 - float64(i)*0.01,
		})
	}
	pool = append(pool,
		Passage{DocumentID: "gold_a", Text: "token accounting discrepancy Jira SUP tickets", Score: 0.15},
		Passage{DocumentID: "gold_b", Text: "retention exception Northstar Bank payload 0 days", Score: 0.12},
	)
	// Shuffle golds into the middle of ranking by score-based retain order —
	// coverage/retain use list order; put golds at end so only progressive protect saves them.
	win, diag := progressiveRetainWindow(pool, "Which customers have retention exceptions?", 4, 4,
		[]string{"gold_a", "gold_b"})
	ids := map[string]bool{}
	for _, p := range win {
		ids[p.DocumentID] = true
	}
	if !ids["gold_a"] || !ids["gold_b"] {
		t.Fatalf("gold dropped through shrink: win=%v diag=%v", uniqueDSIDs(win), diag)
	}
	if len(win) > 8 {
		t.Fatalf("window too wide after shrink: %d", len(win))
	}
}

func TestStoragePassageCharsWiderThanPrompt(t *testing.T) {
	if storagePassageChars(2400) <= 2400 {
		t.Fatalf("storage must exceed prompt budget, got %d", storagePassageChars(2400))
	}
	if storagePassageChars(2400) > 8000 {
		t.Fatalf("storage cap 8k, got %d", storagePassageChars(2400))
	}
}

func TestShrinkKeepsMultiChunkPerDoc(t *testing.T) {
	// Date-seeking: two chunks same gold doc must both survive shrink.
	q := "What date did procurement first communicate the company-wide spending freeze?"
	window := []Passage{
		{DocumentID: "gold", ChunkID: "g0", Text: "Intro Deepwell EU finance ranking opportunity.", Score: 0.9},
		{DocumentID: "gold", ChunkID: "g1", Text: "2026-01-20: Procurement company-wide budget freeze reported.", Score: 0.5},
		{DocumentID: "n1", Text: "noise a", Score: 0.95},
		{DocumentID: "n2", Text: "noise b", Score: 0.94},
		{DocumentID: "n3", Text: "noise c", Score: 0.93},
	}
	out := shrinkWindowKeepProtected(window, q, 4, []string{"gold"})
	var goldChunks int
	for _, p := range out {
		if p.DocumentID == "gold" {
			goldChunks++
		}
	}
	if goldChunks < 2 {
		t.Fatalf("want 2 gold chunks after shrink, got %d out=%v", goldChunks, uniqueDSIDs(out))
	}
	if !containsDate(out, "2026-01-20") {
		t.Fatalf("date chunk dropped: %+v", out)
	}
}

func containsDate(ps []Passage, d string) bool {
	for _, p := range ps {
		if strings.Contains(p.Text, d) {
			return true
		}
	}
	return false
}

func TestPruneCitationsCap(t *testing.T) {
	cited := []string{"a", "b", "c", "d", "e"}
	out := pruneCitations(cited, nil, "basic")
	if len(out) > 3 {
		t.Fatalf("cap broken: %v", out)
	}
	claims := []Claim{{DocumentID: "c", Text: "t", Quote: "quotehere12"}}
	out2 := pruneCitations(cited, claims, "basic")
	if len(out2) != 1 || out2[0] != "c" {
		t.Fatalf("claim prefer: %v", out2)
	}
}
