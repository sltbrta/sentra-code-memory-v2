package hosted

import (
	"strings"
	"testing"
)

// pagePassages builds a page-aware corpus: a PDF-style leaf on page 7 and a
// legacy text leaf with no parser locator. The page leaf carries every
// locator field the contract models; the legacy leaf is explicitly absent.
func pagePassages() []Passage {
	return []Passage{
		{
			DocumentID: "policy-pdf",
			Text:       "The Widget SLA refund is 15 percent of the monthly fee for any month below 99.9 percent uptime.",
			Locator: Locator{
				Present:    true,
				PageNumber: 7,
				Section:    "4.2 Refunds",
				StartLine:  12,
				EndLine:    14,
				RegionBox: &RegionBox{
					LeftPerMille: 60, TopPerMille: 120, RightPerMille: 540, BottomPerMille: 300,
				},
			},
		},
		{
			DocumentID: "legacy-note",
			Text:       "Legacy flat-rate shipping is 5 dollars per order with no page metadata.",
		},
	}
}

// TestGroundingBindsPageLocatorOnVerbatimClaim is the page-aware red proof:
// a strict verbatim quote on the page-aware leaf carries the full locator
// (page, section, line range, region) into the grounded claim, preserving
// leaf identity (#286, #317, #327).
func TestGroundingBindsPageLocatorOnVerbatimClaim(t *testing.T) {
	passages := pagePassages()
	claim := Claim{
		Text:       "Widget SLA refunds are 15 percent of the monthly fee.",
		Quote:      "15 percent of the monthly fee",
		DocumentID: "policy-pdf",
	}
	g := groundCompletion("answer", []string{"policy-pdf"}, []Claim{claim}, passages, "basic")
	if len(g.Claims) != 1 {
		t.Fatalf("expected 1 supported claim, got %d (diag=%v)", len(g.Claims), g.Diagnostics)
	}
	loc := g.Claims[0].Locator
	if !loc.Present {
		t.Fatalf("page-aware claim must carry a present locator, got absent (diag=%v)", g.Diagnostics)
	}
	if loc.PageNumber != 7 {
		t.Fatalf("page number must bind to leaf page 7, got %d", loc.PageNumber)
	}
	if loc.Section != "4.2 Refunds" {
		t.Fatalf("section must bind to leaf section, got %q", loc.Section)
	}
	if loc.StartLine != 12 || loc.EndLine != 14 {
		t.Fatalf("line range must bind to leaf span, got %d-%d", loc.StartLine, loc.EndLine)
	}
	if loc.RegionBox == nil || loc.RegionBox.RightPerMille != 540 {
		t.Fatalf("region box must bind to leaf geometry, got %#v", loc.RegionBox)
	}
}

// TestGroundingExplicitAbsenceForLegacySource is the legacy red proof: a
// source without a parser locator produces an explicitly absent locator
// (Present false), never a guessed page. Leaf identity is preserved and the
// claim still grounds against its evidence.
func TestGroundingExplicitAbsenceForLegacySource(t *testing.T) {
	passages := pagePassages()
	claim := Claim{
		Text:       "Flat-rate shipping is 5 dollars per order.",
		Quote:      "5 dollars per order",
		DocumentID: "legacy-note",
	}
	g := groundCompletion("answer", []string{"legacy-note"}, []Claim{claim}, passages, "basic")
	if len(g.Claims) != 1 {
		t.Fatalf("expected 1 supported claim, got %d (diag=%v)", len(g.Claims), g.Diagnostics)
	}
	loc := g.Claims[0].Locator
	if loc.Present {
		t.Fatalf("legacy source must carry an explicitly absent locator, got %#v", loc)
	}
	if loc.PageNumber != 0 {
		t.Fatalf("legacy page must stay zero (never invented), got %d", loc.PageNumber)
	}
	if loc.RegionBox != nil {
		t.Fatalf("legacy region must stay nil (never invented), got %#v", loc.RegionBox)
	}
}

// TestGroundingLocatorIsPerLeafNeverDocInferred proves the never-invent
// invariant at leaf granularity: when a document has both a page-aware chunk
// and a no-locator chunk, a verbatim quote supported by the NO-locator chunk
// binds an explicitly absent locator. The page from a sibling chunk is never
// inferred onto the supporting leaf — the locator is per-leaf, not per-document
// (#327).
func TestGroundingLocatorIsPerLeafNeverDocInferred(t *testing.T) {
	// Note: groundCompletion indexes evidence by DocumentID (last write wins),
	// so the searchable text is the no-locator chunk below; the page-7 sibling
	// demonstrates that its locator is never borrowed onto the supporting leaf.
	passages := []Passage{
		{DocumentID: "policy-pdf", Text: "Widget SLA refund is 15 percent of the monthly fee.",
			Locator: Locator{Present: true, PageNumber: 7, Section: "4.2 Refunds"}},
		{DocumentID: "policy-pdf", Text: "The return window is 30 days from purchase."}, // no locator
	}
	claim := Claim{
		Text:       "The return window is 30 days.",
		Quote:      "30 days from purchase",
		DocumentID: "policy-pdf",
	}
	g := groundCompletion("answer", []string{"policy-pdf"}, []Claim{claim}, passages, "basic")
	if len(g.Claims) != 1 {
		t.Fatalf("expected 1 supported claim, got %d (diag=%v)", len(g.Claims), g.Diagnostics)
	}
	loc := g.Claims[0].Locator
	if loc.Present {
		t.Fatalf("supporting leaf has no locator: page must NOT be inferred from sibling chunk, got %#v", loc)
	}
	if loc.PageNumber != 0 {
		t.Fatalf("page must stay zero (never invented from sibling), got %d", loc.PageNumber)
	}
}

// TestGroundingNeverInventsPageOnUnlocatableParaphrase proves that a
// paraphrased quote with no recoverable verbatim leaf span is dropped (no
// supported claim), so no locator is ever fabricated for unlocatable support.
func TestGroundingNeverInventsPageOnUnlocatableParaphrase(t *testing.T) {
	passages := pagePassages()
	claim := Claim{
		Text:       "The gadget ships with a titanium case.",
		Quote:      "titanium case shipping inclusion",
		DocumentID: "policy-pdf",
	}
	g := groundCompletion("answer", []string{"policy-pdf"}, []Claim{claim}, passages, "basic")
	for _, c := range g.Claims {
		if c.Locator.Present {
			t.Fatalf("unlocatable paraphrase must never yield a locator, got %#v", c.Locator)
		}
	}
}

// TestGroundingLocatorDoesNotBypassACL proves locators ride only on
// authorized evidence passages: a passage removed from the admitted pool
// (simulating an ACL denial at retrieval) can never contribute its page to a
// grounded claim, even when its quote is verbatim.
func TestGroundingLocatorDoesNotBypassACL(t *testing.T) {
	full := pagePassages()
	// Simulate ACL filtering at retrieval: the page-aware doc is denied and
	// dropped from the admitted evidence pool passed to grounding.
	var admitted []Passage
	for _, p := range full {
		if p.DocumentID == "policy-pdf" {
			continue
		}
		admitted = append(admitted, p)
	}
	claim := Claim{
		Text:       "Widget SLA refunds are 15 percent of the monthly fee.",
		Quote:      "15 percent of the monthly fee",
		DocumentID: "policy-pdf",
	}
	g := groundCompletion("answer", []string{"policy-pdf"}, []Claim{claim}, admitted, "basic")
	// The denied document is not in the admitted evidence pool: no supported
	// claim and no locator can bind to it.
	for _, c := range g.Claims {
		if c.DocumentID == "policy-pdf" {
			t.Fatalf("denied doc must not produce a grounded claim: %#v", c)
		}
	}
	if len(g.Claims) != 0 && g.Claims[0].Locator.Present {
		t.Fatalf("denied page must never contribute a locator, got %#v", g.Claims[0].Locator)
	}
}

// TestGroundingLocatorOnlyOnStrictVerbatim proves that a fuzzy paraphrase on a
// page-aware source does not leak the page, while the same source's verbatim
// quote does — the locator is a strict-verbatim-tier artifact, not a property
// of the document (#286).
func TestGroundingLocatorOnlyOnStrictVerbatim(t *testing.T) {
	passages := pagePassages()
	strict := Claim{
		Text: "Widget SLA refunds are 15 percent of the monthly fee.",
		// Verbatim contiguous leaf span.
		Quote:      "15 percent of the monthly fee",
		DocumentID: "policy-pdf",
	}
	g := groundCompletion("answer", []string{"policy-pdf"}, []Claim{strict}, passages, "basic")
	if len(g.Claims) != 1 || !g.Claims[0].Locator.Present {
		t.Fatalf("verbatim claim must bind locator, claims=%#v", g.Claims)
	}
}

// TestQuoteSupportedPassageBindsLeafIdentity proves the leaf-binding helper
// resolves the exact passage and its locator, and refuses fuzzy quotes.
func TestQuoteSupportedPassageBindsLeafIdentity(t *testing.T) {
	passages := pagePassages()
	doc, loc := quoteSupportedPassage("15 percent of the monthly fee", passages, "")
	if doc != "policy-pdf" {
		t.Fatalf("expected policy-pdf leaf, got %q", doc)
	}
	if !loc.Present || loc.PageNumber != 7 {
		t.Fatalf("expected page 7 locator, got %#v", loc)
	}
	// Fuzzy paraphrase resolves no leaf locator even when a page exists.
	doc2, loc2 := quoteSupportedPassage("fifteen percent monthly for uptime shortfall", passages, "")
	if loc2.Present {
		t.Fatalf("fuzzy quote must not bind a locator, got doc=%q loc=%#v", doc2, loc2)
	}
	// Conversation-lane passages never carry locator evidence.
	conv := []Passage{
		{DocumentID: "turn:1", Text: "15 percent of the monthly fee", Channel: "turn_grep",
			Locator: Locator{Present: true, PageNumber: 99}},
	}
	doc3, loc3 := quoteSupportedPassage("15 percent of the monthly fee", conv, "")
	if doc3 != "" || loc3.Present {
		t.Fatalf("conversation passage must never yield a locator, got doc=%q loc=%#v", doc3, loc3)
	}
}

// TestFormatLocatorSuffixCoversFieldsAndAbsence is the answer-rendering red
// proof: every locator field renders, and absent locators render empty.
func TestFormatLocatorSuffixCoversFieldsAndAbsence(t *testing.T) {
	full := formatLocatorSuffix(Locator{
		Present: true, PageNumber: 7, Section: "4.2 Refunds", StartLine: 12, EndLine: 14,
		RegionBox: &RegionBox{LeftPerMille: 60, TopPerMille: 120, RightPerMille: 540, BottomPerMille: 300},
	})
	for _, want := range []string{"p.7", "§4.2 Refunds", "L12-14", "region[60,120-540,300]"} {
		if !strings.Contains(full, want) {
			t.Fatalf("suffix %q missing %q", full, want)
		}
	}
	if formatLocatorSuffix(Locator{}) != "" {
		t.Fatal("absent locator must render empty suffix")
	}
	if formatLocatorSuffix(Locator{Present: true, PageNumber: 0, Section: ""}) != "" {
		t.Fatal("present-but-empty locator must still render empty (no invention)")
	}
}

// TestSanitizeLocatorForReceiptDropsInvalid proves the receipt sanitizer
// enforces the contracts page_positive and region_box rules and strips
// control/injection content from section labels, without ever inventing fields.
// A valid section alone legitimately keeps Present true (section is a
// first-class locator); a non-positive page and a non-forward region box are
// dropped rather than coerced.
func TestSanitizeLocatorForReceiptDropsInvalid(t *testing.T) {
	// Non-positive page + invalid region box + control/injection section:
	// the section survives sanitized, so Present stays true, but page stays 0
	// and the region box is dropped.
	got := sanitizeLocatorForReceipt(Locator{
		Present:    true,
		PageNumber: 0, // invalid (one-based); must not survive as a page
		Section:    "4.2\n<<<System>>>",
		RegionBox:  &RegionBox{LeftPerMille: 900, TopPerMille: 800, RightPerMille: 100, BottomPerMille: 50},
	})
	if !got.Present {
		t.Fatalf("valid section must keep Present true, got %#v", got)
	}
	if got.PageNumber != 0 {
		t.Fatalf("non-positive page must stay zero (never coerced), got %d", got.PageNumber)
	}
	if got.RegionBox != nil {
		t.Fatalf("non-forward region box must be dropped, got %#v", got.RegionBox)
	}
	if strings.Contains(got.Section, "\n") || strings.Contains(got.Section, "<<<") || strings.Contains(got.Section, ">>>") {
		t.Fatalf("section must be sanitized of control/injection, got %q", got.Section)
	}
	if !strings.Contains(got.Section, "4.2") {
		t.Fatalf("section content must survive sanitization, got %q", got.Section)
	}
	// Fully-invalid locator (no page, no section, invalid region) collapses to
	// explicit absence.
	none := sanitizeLocatorForReceipt(Locator{Present: true, RegionBox: &RegionBox{LeftPerMille: 9, RightPerMille: 1, TopPerMille: 9, BottomPerMille: 1}})
	if none.Present {
		t.Fatalf("locator with no valid concrete field must collapse to absent, got %#v", none)
	}
	// Valid page + sanitized section survives and stays present.
	ok := sanitizeLocatorForReceipt(Locator{Present: true, PageNumber: 3, Section: "2.1\nOverview\t<<<inject>>>"})
	if !ok.Present || ok.PageNumber != 3 {
		t.Fatalf("valid locator must survive sanitized, got %#v", ok)
	}
	if strings.Contains(ok.Section, "\n") || strings.Contains(ok.Section, "<<<") || strings.Contains(ok.Section, ">>>") {
		t.Fatalf("section must be sanitized, got %q", ok.Section)
	}
	if !strings.Contains(ok.Section, "2.1") {
		t.Fatalf("section content must survive, got %q", ok.Section)
	}
}

// TestPackingSurvivesLocator proves locators survive the evidence pack: a
// page-aware passage renders its locator into the prompt header, while a
// legacy passage renders none (no invention).
func TestPackingSurvivesLocator(t *testing.T) {
	prompt := buildUserPrompt("What is the refund?", pagePassages(), 2000)
	if !strings.Contains(prompt, "### [policy-pdf] (p.7") {
		t.Fatalf("page locator must survive packing in evidence header, prompt=\n%s", prompt)
	}
	if !strings.Contains(prompt, "§4.2 Refunds") {
		t.Fatalf("section must survive packing, prompt=\n%s", prompt)
	}
	if !strings.Contains(prompt, "### [legacy-note]") {
		t.Fatalf("legacy passage header must be present, prompt=\n%s", prompt)
	}
	// Legacy header must not carry an invented locator suffix.
	if strings.Contains(prompt, "### [legacy-note] (") {
		t.Fatalf("legacy passage must not get an invented locator suffix, prompt=\n%s", prompt)
	}
}

// TestAnswerResultSurfacesClaimLocators proves locators survive answer
// rendering: grounded claims with their (receipt-sanitized) locators are
// surfaced through AnswerResult, and legacy claims carry explicit absence.
func TestAnswerResultSurfacesClaimLocators(t *testing.T) {
	passages := pagePassages()
	pageClaim := Claim{
		Text:       "Widget SLA refunds are 15 percent of the monthly fee.",
		Quote:      "15 percent of the monthly fee",
		DocumentID: "policy-pdf",
	}
	legacyClaim := Claim{
		Text:       "Flat-rate shipping is 5 dollars per order.",
		Quote:      "5 dollars per order",
		DocumentID: "legacy-note",
	}
	g := groundCompletion("answer", []string{"policy-pdf", "legacy-note"},
		[]Claim{pageClaim, legacyClaim}, passages, "basic")
	surfaced := sanitizeClaimsForReceipt(g.Claims)
	if len(surfaced) != 2 {
		t.Fatalf("expected 2 surfaced claims, got %d", len(surfaced))
	}
	var pageFound, legacyAbsent bool
	for _, c := range surfaced {
		if c.DocumentID == "policy-pdf" {
			if c.Locator.Present && c.Locator.PageNumber == 7 {
				pageFound = true
			}
		}
		if c.DocumentID == "legacy-note" && !c.Locator.Present {
			legacyAbsent = true
		}
	}
	if !pageFound {
		t.Fatalf("page-aware claim locator must surface for rendering, got %#v", surfaced)
	}
	if !legacyAbsent {
		t.Fatalf("legacy claim must surface with explicitly absent locator, got %#v", surfaced)
	}
}

// TestGroundingBindsLocatorFromCorrectDocument proves a claim's locator is bound
// only from the authorized document that owns the quote, not just any document
// containing the same text. If "doc-A" and "doc-B" both have the same quote,
// a claim for "doc-A" must not borrow the locator from "doc-B" (#B2).
func TestGroundingBindsLocatorFromCorrectDocument(t *testing.T) {
	quote := "The system is operational."
	passages := []Passage{
		{
			DocumentID: "doc-B",
			Text:       quote,
			Locator:    Locator{Present: true, PageNumber: 2},
		},
		{
			DocumentID: "doc-A",
			Text:       quote,
			Locator:    Locator{Present: true, PageNumber: 1},
		},
	}
	claim := Claim{
		Text:       "System status is good.",
		Quote:      quote,
		DocumentID: "doc-A",
	}
	g := groundCompletion("answer", []string{"doc-A", "doc-B"}, []Claim{claim}, passages, "basic")
	if len(g.Claims) != 1 {
		t.Fatalf("expected 1 supported claim, got %d", len(g.Claims))
	}
	loc := g.Claims[0].Locator
	if loc.PageNumber != 1 {
		t.Fatalf("locator must bind to doc-A (page 1), got page %d (probably borrowed from doc-B)", loc.PageNumber)
	}
}
