package hosted

import (
	"strings"
	"unicode"

	"github.com/sltbrta/sentra-code-memory-v2/services/internal/textbound"
)

// The reranker saw the first 1,500 bytes of every document, which on code is
// the licence header and the imports.
//
// That is a window policy, not a size problem, and the two need separating.
// Raising the limit costs the provider proportionally and still shows the
// reranker the top of the file; what the reranker needs is the part of the
// document the query is about. On a source file the answer-bearing definition
// is routinely past the head window, so the model scored a licence header
// against a question about a function.
//
// rerankWindow keeps the head when the query is answered there, and otherwise
// returns a window centred on the best query match, snapped to line
// boundaries. The size is unchanged, so nothing costs more.
//
// Measured offline against LexicalReranker, which is this product's own
// fallback reranker and needs no credentials -- see rerank_window_test.go for
// the numbers. The remote lane cannot be measured here, and this does not
// claim to have measured it: what is shown is that the head-window policy is
// what loses the answer, on a reranker whose scoring is fully inspectable.

// rerankWindowContext is how much of the surrounding document is kept either
// side of a match, as a fraction of the budget. A match is more useful with
// its neighbourhood than as a bare line: a function signature means little
// without the body that follows it.
const rerankWindowLeadFraction = 4

// rerankWindow returns at most limit bytes of text, preferring the region the
// query is about.
func rerankWindow(text, query string, limit int) string {
	if limit <= 0 {
		return ""
	}
	if len(text) <= limit {
		return text
	}
	head := textbound.Bytes(text, limit)
	offset, ok := bestQueryOffset(text, query)
	if !ok || offset < len(head) {
		// The query is answered inside the head window, or not present at all.
		// Keeping the head is both cheaper to reason about and what every
		// existing receipt digest was computed over.
		return head
	}
	return windowAround(text, offset, limit)
}

// bestQueryOffset returns the byte offset of the earliest occurrence of the
// longest query token that appears in text.
//
// The longest token is used rather than the first: query words like "the" or
// "func" match everywhere and would centre the window on noise. Matching is
// case-insensitive on the token, which is how identifiers are written about.
func bestQueryOffset(text, query string) (int, bool) {
	tokens := strings.FieldsFunc(query, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsNumber(r) && r != '_'
	})
	best, bestLen := -1, 0
	lowerText := strings.ToLower(text)
	for _, token := range tokens {
		if len(token) < 3 || len(token) <= bestLen {
			continue
		}
		if index := strings.Index(lowerText, strings.ToLower(token)); index >= 0 {
			best, bestLen = index, len(token)
		}
	}
	if best < 0 {
		return 0, false
	}
	return best, true
}

// windowAround returns limit bytes of text centred on offset, snapped to line
// boundaries so the reranker never sees a half-line, and to rune boundaries so
// the result is valid UTF-8.
func windowAround(text string, offset, limit int) string {
	lead := limit / rerankWindowLeadFraction
	start := offset - lead
	if start < 0 {
		start = 0
	}
	if start > 0 {
		// Snap forward to the start of the next line: beginning mid-statement
		// reads as a truncated fragment to any scorer.
		if next := strings.IndexByte(text[start:], '\n'); next >= 0 && next < lead {
			start += next + 1
		}
	}
	end := start + limit
	if end > len(text) {
		end = len(text)
		if start > end-limit && end-limit > 0 {
			start = end - limit
		}
	}
	window := text[start:end]
	// Drop a trailing partial line for the same reason as the leading one, but
	// only when there is enough left for it to matter.
	if last := strings.LastIndexByte(window, '\n'); last > len(window)/2 {
		window = window[:last]
	}
	return textbound.Bytes(window, limit)
}
