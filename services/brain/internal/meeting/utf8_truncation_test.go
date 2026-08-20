package meeting

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// Meeting statements and prose are truncated into proto3 string fields, and
// proto.Marshal rejects invalid UTF-8 outright. A byte-offset cut therefore did
// not degrade the answer for a non-English transcript -- it failed the request
// at the serialization boundary.

func TestTruncateNeverProducesInvalidUTF8(t *testing.T) {
	inputs := []string{
		"the meeting discussed the roadmap",
		"会議ではロードマップについて議論しました",
		"la réunion a porté sur la feuille de route",
		"обсуждение дорожной карты",
		"mixed 日本語 and ASCII and 🎉 emoji",
	}
	for _, input := range inputs {
		for max := 0; max <= len(input)+2; max++ {
			got := truncate(input, max)
			if !utf8.ValidString(got) {
				t.Fatalf("truncate(%q, %d) = %q: invalid UTF-8 would fail proto.Marshal", input, max, got)
			}
			if len(got) > max && max > 0 {
				t.Fatalf("truncate(%q, %d) = %q exceeds the limit", input, max, got)
			}
			if !strings.HasPrefix(input, got) {
				t.Fatalf("truncate(%q, %d) = %q is not a prefix", input, max, got)
			}
		}
	}
}
