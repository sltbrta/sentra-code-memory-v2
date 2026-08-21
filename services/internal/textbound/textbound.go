// Package textbound truncates text without splitting a character.
//
// About a dozen private `truncate` helpers across this repository did `s[:n]`.
// A byte offset lands mid-rune on any non-ASCII input, and the consequences
// differ by destination:
//
//   - Into a proto3 string field, invalid UTF-8 makes proto.Marshal fail, so a
//     meeting transcript in any non-English language failed at serialization
//     rather than returning a shorter answer.
//   - Into a model provider's JSON body, encoding/json substitutes U+FFFD, so
//     the last token of every truncated document is corrupted.
//   - Into evidence packing, it breaks the verbatim-quote contract the
//     synthesis prompt depends on: a mangled boundary makes the model's quote
//     fail re-verification against the stored passage, quietly lowering
//     citation rates.
//
// The correct implementation already existed as one unexported function in the
// hosted package. This is it, in a place everything can reach.
package textbound

import "unicode/utf8"

// Bytes returns at most limit bytes of s, cut on a rune boundary.
//
// The result is always valid UTF-8 when s is. A limit smaller than the first
// rune yields the empty string rather than a fragment.
//
// When s is *not* valid UTF-8, the result is bounded rather than emptied. The
// retreat is capped at utf8.UTFMax-1 positions, which is what the comment
// below always claimed and what the loop did not do: an unbounded walk back
// over a run of continuation bytes discards everything before it. A fuzz test
// found it -- 3,000 bytes of invalid input with a 2,000-byte limit returned
// zero bytes, so an embedding input, a rerank document or an LLM prompt built
// from a binary or mis-decoded file silently became empty instead of
// truncated, at every one of this package's call sites.
func Bytes(s string, limit int) string {
	if limit <= 0 {
		return ""
	}
	if len(s) <= limit {
		return s
	}
	// Walk back from the limit to the start of the rune that straddles it.
	// A rune is at most 4 bytes, so this retreats at most 3 positions -- and
	// the bound is enforced rather than assumed, because it only holds for
	// valid input and this function is handed whatever was ingested.
	floor := limit - (utf8.UTFMax - 1)
	if floor < 0 {
		floor = 0
	}
	for cut := limit; cut > floor; cut-- {
		if utf8.RuneStart(s[cut]) {
			return s[:cut]
		}
	}
	if utf8.RuneStart(s[floor]) {
		// Includes the documented case where the limit is smaller than the
		// first rune: floor is 0, s[0] starts a rune, and the result is empty
		// rather than a fragment.
		return s[:floor]
	}
	// No rune boundary within a rune's width of the limit, and the retreat did
	// not reach a rune start: s is not valid UTF-8 here. Honour the byte bound
	// the caller asked for rather than returning almost nothing -- the result
	// is no less valid than the input, and an empty one loses everything.
	return s[:limit]
}

// Runes returns at most limit runes of s.
//
// Use this where the bound is a character count rather than a byte budget --
// a display width or a documented "512 characters" limit. Several callers
// compared a rune-documented limit against len(s), so a multibyte query was
// cut at roughly a third of the advertised length.
func Runes(s string, limit int) string {
	if limit <= 0 {
		return ""
	}
	if utf8.RuneCountInString(s) <= limit {
		return s
	}
	count := 0
	for index := range s {
		if count == limit {
			return s[:index]
		}
		count++
	}
	return s
}

// Ellipsis returns at most limit bytes of s, appending a single-character
// ellipsis when anything was removed. The ellipsis is counted against limit, so
// the result never exceeds it.
func Ellipsis(s string, limit int) string {
	const mark = "…"
	if limit <= 0 {
		return ""
	}
	if len(s) <= limit {
		return s
	}
	if limit <= len(mark) {
		return Bytes(s, limit)
	}
	return Bytes(s, limit-len(mark)) + mark
}
