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
func Bytes(s string, limit int) string {
	if limit <= 0 {
		return ""
	}
	if len(s) <= limit {
		return s
	}
	// Walk back from the limit to the start of the rune that straddles it.
	// A rune is at most 4 bytes, so this retreats at most 3 positions.
	cut := limit
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
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
