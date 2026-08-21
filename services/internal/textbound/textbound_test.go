package textbound_test

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/sltbrta/sentra-code-memory-v2/services/internal/textbound"
)

// Byte-offset truncation was the shape everywhere: s[:n]. These pin that the
// output is always valid UTF-8, which is what proto3 marshalling, JSON encoding
// and the verbatim-quote contract all require.

func TestBytesAlwaysReturnsValidUTF8(t *testing.T) {
	inputs := []string{
		"ascii only",
		"café society",    // 2-byte rune
		"日本語のテキスト",        // 3-byte runes
		"emoji 🎉🎊🎈 party", // 4-byte runes
		"mixed ünïcödé 日本 🎉 text",
		strings.Repeat("é", 100),
	}
	for _, input := range inputs {
		for limit := 0; limit <= len(input)+2; limit++ {
			got := textbound.Bytes(input, limit)
			if !utf8.ValidString(got) {
				t.Fatalf("Bytes(%q, %d) = %q: not valid UTF-8", input, limit, got)
			}
			if len(got) > limit {
				t.Fatalf("Bytes(%q, %d) = %q: %d bytes exceeds the limit", input, limit, got, len(got))
			}
			if !strings.HasPrefix(input, got) {
				t.Fatalf("Bytes(%q, %d) = %q: not a prefix of the input", input, limit, got)
			}
		}
	}
}

func TestBytesReturnsTheWholeStringWhenItFits(t *testing.T) {
	const s = "日本語"
	if got := textbound.Bytes(s, len(s)); got != s {
		t.Fatalf("Bytes(%q, %d) = %q, want the whole string", s, len(s), got)
	}
}

func TestRunesCountsCharactersNotBytes(t *testing.T) {
	const s = "日本語のテキスト" // 8 runes, 24 bytes
	got := textbound.Runes(s, 3)
	if utf8.RuneCountInString(got) != 3 {
		t.Fatalf("Runes(%q, 3) = %q: %d runes, want 3", s, got, utf8.RuneCountInString(got))
	}
	if !utf8.ValidString(got) {
		t.Fatalf("Runes produced invalid UTF-8: %q", got)
	}
}

func TestEllipsisNeverExceedsItsLimit(t *testing.T) {
	const s = "日本語のテキストがここにあります"
	for limit := 1; limit < len(s); limit++ {
		got := textbound.Ellipsis(s, limit)
		if len(got) > limit {
			t.Fatalf("Ellipsis(%q, %d) = %q: %d bytes exceeds the limit", s, limit, got, len(got))
		}
		if !utf8.ValidString(got) {
			t.Fatalf("Ellipsis(%q, %d) = %q: not valid UTF-8", s, limit, got)
		}
	}
}

// FuzzBytesAlwaysValid is the amplification pass: whatever bytes and limit
// arrive, the result must be valid UTF-8 and within the bound.
func FuzzBytesAlwaysValid(f *testing.F) {
	// Invalid-UTF-8 seeds: the retreat to a rune boundary used to walk back
	// over every continuation byte, so input with no rune start emptied
	// entirely. Found by a fuzz test in another package that was trying to
	// prove something unrelated.
	f.Add(strings.Repeat("\xa1", 3000), 2000)
	f.Add(strings.Repeat("a", 100)+strings.Repeat("\xa1", 2900), 2000)
	f.Add("\x80\x81\x82\x83", 2)
	for _, seed := range []string{"", "a", "café", "日本語", "🎉", "mixed ünï 日本 🎉"} {
		f.Add(seed, 3)
	}
	f.Fuzz(func(t *testing.T, s string, limit int) {
		got := textbound.Bytes(s, limit)
		if limit > 0 && len(got) > limit {
			t.Fatalf("Bytes(%q, %d) = %q exceeds the limit", s, limit, got)
		}
		if utf8.ValidString(s) && !utf8.ValidString(got) {
			t.Fatalf("Bytes(%q, %d) = %q: valid input produced invalid output", s, limit, got)
		}
		if !strings.HasPrefix(s, got) {
			t.Fatalf("Bytes(%q, %d) = %q: not a prefix", s, limit, got)
		}
		// The retreat is at most one rune's width. Without this the walk back
		// over continuation bytes is unbounded, and input with no rune start
		// empties entirely -- which is how this was found.
		if limit > 0 && limit < len(s) {
			if retreat := limit - len(got); retreat > utf8.UTFMax-1 {
				t.Fatalf("Bytes(%q, %d) retreated %d bytes; a rune is at most %d",
					s, limit, retreat, utf8.UTFMax)
			}
		}
	})
}

// Bytes emptied its input rather than bounding it, whenever the input was not
// valid UTF-8.
//
// The retreat to a rune boundary was an unbounded walk back over any byte that
// is not a rune start. A run of continuation bytes -- a binary file, a
// mis-decoded document, anything that is not text -- has no rune start to find,
// so the walk ran to the beginning of the string and returned almost nothing.
// Measured before the fix: 3,000 bytes with a 2,000-byte limit returned 0.
//
// This function is on the path to an embedding provider, a reranker and three
// LLM prompts. Silently sending an empty string is worse than sending a
// truncated one, and nothing downstream could tell the difference.
//
// A fuzz test in the ontology package found it, while trying to prove an
// unrelated claim about a different truncation site.

func TestBytesBoundsInvalidInputRatherThanEmptyingIt(t *testing.T) {
	for name, tc := range map[string]struct {
		input   string
		limit   int
		wantMin int
	}{
		// No rune start anywhere: the old walk ran to zero.
		"all continuation bytes": {strings.Repeat("\xa1", 3000), 2000, 1997},
		"binary-ish":             {strings.Repeat("\x80\x81\x82", 1000), 2000, 1997},
		// Valid prefix then garbage: the old walk discarded the valid part too.
		"valid then garbage": {strings.Repeat("a", 100) + strings.Repeat("\xa1", 2900), 2000, 1997},
	} {
		t.Run(name, func(t *testing.T) {
			got := textbound.Bytes(tc.input, tc.limit)
			if len(got) > tc.limit {
				t.Fatalf("returned %d bytes, over the %d limit", len(got), tc.limit)
			}
			if len(got) < tc.wantMin {
				t.Fatalf("returned %d bytes of a %d-byte input bounded at %d: "+
					"invalid input is being emptied rather than truncated, and "+
					"every caller sends the result to a model provider",
					len(got), len(tc.input), tc.limit)
			}
		})
	}
}

// TestBytesRetreatsAtMostARuneWidth is the property the comment always claimed
// and the loop did not enforce.
func TestBytesRetreatsAtMostARuneWidth(t *testing.T) {
	const limit = 500
	for name, input := range map[string]string{
		"valid multibyte": strings.Repeat("界", 400),
		"valid ascii":     strings.Repeat("a", 1000),
		"invalid":         strings.Repeat("\xa1", 1000),
		"mixed":           strings.Repeat("界", 100) + strings.Repeat("\x80", 900),
	} {
		t.Run(name, func(t *testing.T) {
			got := textbound.Bytes(input, limit)
			if retreat := limit - len(got); retreat > utf8.UTFMax-1 {
				t.Fatalf("retreated %d bytes; a rune is at most %d, so the walk "+
					"is unbounded on input that has no rune start",
					retreat, utf8.UTFMax)
			}
		})
	}
}

// TestBytesStillEmptiesWhenTheLimitIsSmallerThanTheFirstRune keeps the
// documented case: a limit that cannot hold one rune yields the empty string
// rather than a fragment. The fix must not turn that into a broken rune.
func TestBytesStillEmptiesWhenTheLimitIsSmallerThanTheFirstRune(t *testing.T) {
	for _, tc := range []struct {
		input string
		limit int
	}{
		{"🎉", 1}, {"🎉", 2}, {"🎉", 3},
		{"界", 1}, {"界", 2},
		{"日本語のテキスト", 1},
	} {
		got := textbound.Bytes(tc.input, tc.limit)
		if got != "" {
			t.Errorf("textbound.Bytes(%q, %d) = %q, want empty: a limit smaller than the "+
				"first rune must not yield a fragment", tc.input, tc.limit, got)
		}
	}
}
