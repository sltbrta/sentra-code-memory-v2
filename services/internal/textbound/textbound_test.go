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
	})
}
