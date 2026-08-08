package hosted

import (
	"fmt"
	"strings"
	"unicode"
)

// Locator carries the page/section/line/offset/region metadata that a
// layout-aware source parser (#317) attached to one leaf evidence span. Every
// field is optional; Present is the single source of truth for whether any
// locator metadata exists, so a missing locator is explicit and is never
// confused with a zero-valued page number.
//
// Locators are observed facts carried from authorized passages. The grounding
// path binds them only to claims whose quote is a strict verbatim leaf span,
// and never invents or infers a page/region (#327). A model-proposed locator
// is never trusted — the authorized passage's locator always wins.
type Locator struct {
	// Present is true only when the parser attached at least one locator
	// field. False is the explicit-absence sentinel; it is never set by the
	// grounding path on its own.
	Present bool `json:"present,omitempty"`
	// PageNumber is one-based when the source has pages (PDF, slide deck). A
	// zero value means "no page"; the grounding path never promotes it.
	PageNumber uint32 `json:"page_number,omitempty"`
	// Section is a parser-supplied section/heading label.
	Section string `json:"section,omitempty"`
	// StartLine/StartColumn..EndLine/EndColumn is a one-based half-open text
	// range within the rendered source, mirroring the contracts SourceRange.
	StartLine   uint32 `json:"start_line,omitempty"`
	StartColumn uint32 `json:"start_column,omitempty"`
	EndLine     uint32 `json:"end_line,omitempty"`
	EndColumn   uint32 `json:"end_column,omitempty"`
	// RegionBox is an optional page-relative geometry, mirroring NormalizedBox.
	RegionBox *RegionBox `json:"region_box,omitempty"`
}

// RegionBox is a normalized per-mille page rectangle, mirroring the contracts
// NormalizedBox. A forward box has left<right and top<bottom within [0,1000].
type RegionBox struct {
	LeftPerMille   uint32 `json:"left_per_mille,omitempty"`
	TopPerMille    uint32 `json:"top_per_mille,omitempty"`
	RightPerMille  uint32 `json:"right_per_mille,omitempty"`
	BottomPerMille uint32 `json:"bottom_per_mille,omitempty"`
}

// regionBoxForward reports whether a box is a valid forward per-mille rect,
// mirroring the contracts multimodal_evidence_item.region_box CEL rule.
func regionBoxForward(b RegionBox) bool {
	return b.LeftPerMille < b.RightPerMille &&
		b.TopPerMille < b.BottomPerMille &&
		b.RightPerMille <= 1000 && b.BottomPerMille <= 1000
}

// hasLocatorField reports whether any concrete locator field is populated.
func hasLocatorField(loc Locator) bool {
	return loc.PageNumber > 0 ||
		strings.TrimSpace(loc.Section) != "" ||
		loc.StartLine > 0 || loc.StartColumn > 0 ||
		loc.EndLine > 0 || loc.EndColumn > 0 ||
		(loc.RegionBox != nil && regionBoxForward(*loc.RegionBox))
}

// normalizeLocator enforces the explicit-absence invariant and the contracts'
// anchor validity rules (page_positive, region_box). It drops Present when no
// concrete field remains, drops a non-positive page, drops an invalid region
// box, and sanitizes+bounds the section label. It never adds fields — absence
// stays absence, so a page is never invented (#327).
func normalizeLocator(loc Locator) Locator {
	loc.Section = sanitizeLocatorSection(loc.Section)
	if loc.RegionBox != nil && !regionBoxForward(*loc.RegionBox) {
		loc.RegionBox = nil
	}
	loc.Present = hasLocatorField(loc)
	return loc
}

// sanitizeLocatorSection returns a bounded, control-free section label. It
// strips newlines/tabs and other control runes, bounds the label to keep
// receipts/diagnostics compact, and defangs prompt-injection fence markers so
// a parser-supplied heading cannot impersonate prompt structure.
func sanitizeLocatorSection(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	var b strings.Builder
	b.Grow(len(s))
	n := 0
	for _, r := range s {
		if r == '\n' || r == '\t' || unicode.IsControl(r) {
			continue
		}
		if n >= 64 {
			break
		}
		b.WriteRune(r)
		n++
	}
	out := b.String()
	out = strings.ReplaceAll(out, "<<<", "«")
	out = strings.ReplaceAll(out, ">>>", "»")
	return out
}

// sanitizeLocatorForReceipt returns a bounded, receipt-safe copy of loc. It
// runs normalizeLocator so receipts and diagnostics never carry an invented
// page, an invalid region box, or an unbounded/control-laden section label
// (#327).
func sanitizeLocatorForReceipt(loc Locator) Locator {
	return normalizeLocator(loc)
}

// sanitizeClaimsForReceipt returns a copy of claims with each locator
// receipt-sanitized. It never drops a verified claim and never invents a
// locator; it only normalizes the locator fields already bound by grounding.
func sanitizeClaimsForReceipt(claims []Claim) []Claim {
	if len(claims) == 0 {
		return nil
	}
	out := make([]Claim, 0, len(claims))
	for _, c := range claims {
		c.Locator = sanitizeLocatorForReceipt(c.Locator)
		out = append(out, c)
	}
	return out
}

// formatLocatorSuffix renders a human-readable citation locator suffix for
// answer/prompt rendering, or "" when loc is absent. It emits only fields the
// parser attached and never invents a page number or region (#327). The
// returned form begins with a leading space and is wrapped in parentheses,
// e.g. " (p.3, §2.1, L12-14, region[10,20-110,220])". Callers that need only
// the inner form should trim the surrounding space/parens.
func formatLocatorSuffix(loc Locator) string {
	loc = normalizeLocator(loc)
	if !loc.Present {
		return ""
	}
	var parts []string
	if loc.PageNumber > 0 {
		parts = append(parts, fmt.Sprintf("p.%d", loc.PageNumber))
	}
	if loc.Section != "" {
		parts = append(parts, "§"+loc.Section)
	}
	if loc.StartLine > 0 {
		if loc.EndLine > loc.StartLine {
			parts = append(parts, fmt.Sprintf("L%d-%d", loc.StartLine, loc.EndLine))
		} else {
			parts = append(parts, fmt.Sprintf("L%d", loc.StartLine))
		}
	}
	if loc.RegionBox != nil {
		parts = append(parts, fmt.Sprintf("region[%d,%d-%d,%d]",
			loc.RegionBox.LeftPerMille, loc.RegionBox.TopPerMille,
			loc.RegionBox.RightPerMille, loc.RegionBox.BottomPerMille))
	}
	if len(parts) == 0 {
		return ""
	}
	return " (" + strings.Join(parts, ", ") + ")"
}

// quoteSupportedPassage locates the leaf passage whose text contains the
// verbatim (whitespace/case-normalized) quote span and returns its document id
// and locator. It performs strict contiguous matching only — there is no
// n-gram/paraphrase fallback — so a locator is bound only when the supporting
// quote is an exact leaf span. A fuzzy/paraphrased match yields Locator{}
// (Present=false) rather than a guessed page, preserving leaf identity and the
// strict-verbatim citation tier (#286, #327).
//
// The returned locator is the authorized passage's own observed locator; this
// function never invents one. Conversation-lane passages are never evidence.
func quoteSupportedPassage(quote string, passages []Passage, targetDocID string) (docID string, locator Locator) {
	needle := normText(quote)
	minLen := 8
	if hasDigit(quote) || hasUpperRun(quote) {
		minLen = 6
	}
	if len(needle) < minLen {
		return "", Locator{}
	}
	for _, p := range passages {
		if p.DocumentID == "" || isConversationPassage(p) {
			continue
		}
		if targetDocID != "" && p.DocumentID != targetDocID {
			continue
		}
		if strings.Contains(normText(p.Text), needle) {
			return p.DocumentID, normalizeLocator(p.Locator)
		}
	}
	return "", Locator{}
}
