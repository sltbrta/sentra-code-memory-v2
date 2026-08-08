package hosted

import (
	"strings"
	"testing"
)

// Top-ranked evidence must survive prompt truncation even when the pack
// arrives bestLast-ordered (highest-ranked doc at the tail).
func TestBuildUserPromptKeepsTopRankedEvidence(t *testing.T) {
	var passages []Passage
	for i := 0; i < 20; i++ {
		passages = append(passages, Passage{
			DocumentID: docID(i),
			Text:       "body of evidence doc " + docID(i) + " with unique marker token",
			Score:      float64(i) / 100, // bestLast: ascending, best doc last
		})
	}
	out := buildUserPrompt("What is the policy?", passages, 400)
	// 20 docs > 12 → docCap 16; the 4 weakest (d00–d03) are cut, best survive.
	if !strings.Contains(out, "### [d19]") {
		t.Fatalf("top-ranked doc d19 (bestLast tail) dropped from prompt:\n%s", out)
	}
	if !strings.Contains(out, "### [d04]") {
		t.Fatalf("d04 (rank 5) should survive the cut:\n%s", out)
	}
	for _, dropped := range []string{"### [d00]", "### [d01]", "### [d02]", "### [d03]"} {
		if strings.Contains(out, dropped) {
			t.Fatalf("weakest doc %s should have been truncated, not top-ranked ones", dropped)
		}
	}
	// Relative order preserved: bestLast stays best-last among emitted docs.
	if strings.Index(out, "### [d19]") < strings.Index(out, "### [d04]") {
		t.Fatal("emitted order must preserve passed ranking (d19 after d04)")
	}
}

func TestBuildUserPromptCountReflectsActualTruncatedInputs(t *testing.T) {
	passages := make([]Passage, 0, 26)
	for i := 0; i < 20; i++ {
		passages = append(passages, Passage{
			DocumentID: docID(i), Text: "evidence " + docID(i), Score: float64(i + 1),
		})
	}
	for i := 0; i < 6; i++ {
		passages = append(passages, Passage{
			DocumentID: "turn:" + docID(i), Text: "conversation " + docID(i),
		})
	}

	prompt, packed := buildUserPromptOptsWithCount("What is the policy?", passages, 400, "")
	// Twenty documents select the 16-doc window; six conversation passages
	// select the four-snippet window. Both kinds are synthesis inputs.
	if packed != 20 {
		t.Fatalf("packed passage count=%d want 20", packed)
	}
	if got := strings.Count(prompt, "\n### ["); got != 16 {
		t.Fatalf("prompt document count=%d want 16", got)
	}
	if got := strings.Count(prompt, "- [turn:"); got != 4 {
		t.Fatalf("prompt conversation count=%d want 4", got)
	}
}

// Stable fallback: equal scores (e.g. memory fixture path) keep the head,
// matching the pre-fix behavior for unranked packs.
func TestTruncateDocsForPromptPreservesPromptEdges(t *testing.T) {
	passages := make([]Passage, 20)
	for i := range passages {
		passages[i] = Passage{DocumentID: docID(i), Score: float64(i + 1)}
	}
	// The tail is the authority-promoted best document but has a lower raw
	// score; edge preservation must keep it in the prompt candidate set.
	passages[19].Score = 0.01
	out := truncateDocsForPrompt(passages, 16)
	seen := map[string]bool{}
	for _, p := range out {
		seen[p.DocumentID] = true
	}
	if !seen["d19"] {
		t.Fatalf("authority-promoted prompt tail was dropped: %v", out)
	}
}

// Regression: adjudication/rerank reorders the pack without rewriting raw
// Passage.Score. Truncation must follow the adjudicated bestLast order
// (best doc at the tail), not reselect by stale scores.
func TestTruncateDocsForPromptPreservesAdjudicatedOrder(t *testing.T) {
	passages := make([]Passage, 20)
	for i := range passages {
		passages[i] = Passage{DocumentID: docID(i), Score: float64(i + 1)}
	}
	// Authority/adjudication promoted d16-d18 into the pack's top ranks
	// (bestLast tail side) but their raw retrieval scores stayed bottom.
	passages[16].Score = 0.01
	passages[17].Score = 0.02
	passages[18].Score = 0.03
	out := truncateDocsForPrompt(passages, 16)
	if len(out) != 16 {
		t.Fatalf("want 16 docs, got %d", len(out))
	}
	seen := map[string]bool{}
	for _, p := range out {
		seen[p.DocumentID] = true
	}
	// Adjudicated top-16 (positions 4..19 in bestLast order) survive even
	// where raw scores disagree; the four weakest positions are cut.
	for _, kept := range []string{"d04", "d16", "d17", "d18", "d19"} {
		if !seen[kept] {
			t.Fatalf("adjudicated doc %s dropped by stale-score reselection: %v", kept, out)
		}
	}
	for _, dropped := range []string{"d00", "d01", "d02", "d03"} {
		if seen[dropped] {
			t.Fatalf("weakest adjudicated position %s should have been truncated", dropped)
		}
	}
	// Relative order preserved: bestLast stays best-last among emitted docs.
	if out[len(out)-1].DocumentID != "d19" {
		t.Fatalf("emitted order must preserve adjudicated ranking, got tail %s", out[len(out)-1].DocumentID)
	}
}

func TestTruncateDocsForPromptEqualScores(t *testing.T) {
	var passages []Passage
	for i := 0; i < 5; i++ {
		passages = append(passages, Passage{DocumentID: docID(i), Text: "t" + docID(i)})
	}
	out := truncateDocsForPrompt(passages, 3)
	if len(out) != 3 {
		t.Fatalf("want 3 docs, got %d", len(out))
	}
	for i, want := range []string{"d00", "d01", "d02"} {
		if out[i].DocumentID != want {
			t.Fatalf("equal scores must keep stable head order: got %v", out)
		}
	}
	// No-op when under cap.
	if got := truncateDocsForPrompt(passages, 16); len(got) != 5 {
		t.Fatalf("under cap must be a no-op, got %d", len(got))
	}
}

// Issue #376: the system prompt must include the answer formatting guidance
// so the model returns Markdown the safe UI renderer supports, and the JSON
// schema + abstain/false-abstain rules must still be present.
func TestBuildSystemPromptIncludesAnswerFormatGuidance(t *testing.T) {
	prompt := buildSystemPromptOpts("basic", []string{"text"}, "What is the RPO?")
	for _, must := range []string{
		"Answer only from supplied documents",      // base
		answerFormatGuidance,                       // new formatting block
		"lead with the direct answer",              // directive
		"clean Markdown paragraphs",                // rendering target
		"NEVER include raw citation markers",       // forbid raw markers
		"cited_document_ids",                       // JSON contract preserved
		"claims",                                   // JSON contract preserved
		"Type guidance: " + hardTypeNotes["basic"], // type note still applied
	} {
		if !strings.Contains(prompt, must) {
			t.Fatalf("system prompt missing %q", must)
		}
	}
	// The formatting guidance must forbid the raw markers the safe renderer
	// would otherwise echo back as literal text.
	for _, banned := range []string{
		"fenced code blocks",
		"raw HTML",
	} {
		if !strings.Contains(prompt, banned) {
			t.Fatalf("system prompt must forbid %q", banned)
		}
	}
}

// The system prompt must request concise, complete answers (2–5 sentences
// or ≤6 bullets normally, no repetition, complete final sentence) without
// weakening the grounding, completeness, or fail-closed contracts.
//
// Reconciliation: short by default (2–5 sentences / ≤6 items), exhaustive
// when the question asks for enumeration/completeness — the cap MUST NOT
// apply to enumeration answers, and every asked sub-part MUST be covered.
func TestBuildSystemPromptIncludesConcisenessGuidance(t *testing.T) {
	prompt := buildSystemPromptOpts("basic", []string{"text"}, "What is the RPO?")
	for _, must := range []string{
		answerConcisenessGuidance,         // new length block
		"concise and complete",            // directive
		"2–5 sentences",                   // normal prose bound
		"at most 6 items",                 // normal list bound
		"State each fact once",            // no repetition
		"complete final sentence",         // never trail off
		"MUST list EVERY matching entity", // exhaustive enumeration carve-out
	} {
		if !strings.Contains(prompt, must) {
			t.Fatalf("system prompt missing conciseness directive %q", must)
		}
	}
	// Grounding / completeness / fail-closed behavior must survive alongside
	// the length guidance.
	for _, kept := range []string{
		packDiscipline,   // completeness + verbatim facts
		antiFalseAbstain, // fail-closed abstain contract
		jsonSchema,       // JSON contract
		"Enumerate every distinct relevant fact asked; do not omit sub-parts",
		"When abstaining, cited_document_ids MUST be []",
	} {
		if !strings.Contains(prompt, kept) {
			t.Fatalf("conciseness guidance must not weaken %q", kept)
		}
	}
	// Exhaustive enumeration types keep their every-entity override.
	full := buildSystemPromptOpts("completeness", []string{"text"}, "List all customers.")
	if !strings.Contains(full, "enumerate EVERY matching entity") {
		t.Fatal("completeness type guidance must survive conciseness guidance")
	}
}

// Issue #376: source-mode preambles must still be applied alongside the
// formatting guidance, so the answer is both properly formatted and mode-aware.
func TestBuildSystemPromptAppliesFormatAndModePreamble(t *testing.T) {
	prompt := buildSystemPromptOpts("basic", []string{"slack"}, "What did Alice commit to?")
	if !strings.Contains(prompt, answerFormatGuidance) {
		t.Fatal("formatting guidance missing under transcript mode")
	}
	if !strings.Contains(prompt, "Mode: transcript/chat sources") {
		t.Fatal("transcript mode preamble missing alongside formatting guidance")
	}
}

func docID(i int) string {
	return "d" + string(rune('0'+i/10)) + string(rune('0'+i%10))
}
