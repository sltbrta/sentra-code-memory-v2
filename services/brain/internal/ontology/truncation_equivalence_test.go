package ontology

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// extractMentionTokens was one of sixteen inline `s = s[:N]` truncations a
// fresh sweep found, and it is the one that turned out **not** to be a defect.
//
// A byte offset does land mid-rune here. It has no observable consequence,
// because the tokenizer ranges over the string: `for _, r := range body`
// decodes an invalid byte as U+FFFD, which is neither a letter, a digit nor a
// separator, so it ends the current token instead of joining it. The partial
// rune is discarded either way.
//
// A first attempt asserted the output was valid UTF-8, passed against the
// unfixed code, and was deleted rather than kept as evidence it was not. What
// is actually true is narrower and provable: switching this site to
// textbound.Bytes changed nothing. This asserts that, so the claim in the
// ledger matches the code.

// byteCutMentionTokens is the pre-fix implementation, kept here as the thing
// being compared against. It is the only copy; the production path uses
// textbound.
func byteCutMentionTokens(body string) []string {
	if len(body) > 2_000 {
		body = body[:2_000]
	}
	return mentionTokensOf(body)
}

// runeCutMentionTokens is the current implementation's bound, applied to the
// same tokenizer.
func runeCutMentionTokens(body string) []string {
	return extractMentionTokens(body)
}

func TestMentionTokenTruncationIsBehaviourPreserving(t *testing.T) {
	cases := map[string]string{
		// The shapes that make a mid-rune cut reachable at all: a token that
		// would survive the filter (letter plus separator) and runs past the
		// 2,000-byte bound on a three-byte rune.
		"separator token of three-byte runes": "svc_" + strings.Repeat("界", 700),
		"digit token of three-byte runes":     "v2" + strings.Repeat("界", 700),
		"four-byte runes":                     "svc_" + strings.Repeat("𝄞", 520),
		"two-byte runes":                      "svc_" + strings.Repeat("é", 1100),
		"mixed widths":                        strings.Repeat("id_1 界é𝄞 name-2 ", 200),
		"ascii past the bound":                strings.Repeat("auth_service v2 ", 200),
		"exactly at the bound":                strings.Repeat("a", 1_996) + "_界",
		"one byte over":                       strings.Repeat("a", 1_997) + "_界",
		"two bytes over":                      strings.Repeat("a", 1_998) + "_界",
		"under the bound":                     "svc_界 name-1",
		"empty":                               "",
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			before := byteCutMentionTokens(body)
			after := runeCutMentionTokens(body)
			if len(before) != len(after) {
				t.Fatalf("token count changed: %d before, %d after", len(before), len(after))
			}
			for i := range before {
				if before[i] != after[i] {
					t.Fatalf("token %d changed: %q -> %q", i, before[i], after[i])
				}
			}
			// The property the deleted test tried to assert, stated where it
			// is actually true: whichever bound is used, no emitted token
			// carries a broken rune, because the tokenizer discards it.
			for _, tok := range after {
				if !utf8.ValidString(tok) {
					t.Fatalf("token %q is not valid UTF-8", tok)
				}
			}
		})
	}
}

// FuzzMentionTokenTruncationIsBehaviourPreserving widens the claim past the
// cases anyone thought to write down. A difference here would mean the site
// was a defect after all, and the ledger entry would need to change.
func FuzzMentionTokenTruncationIsBehaviourPreserving(f *testing.F) {
	for _, seed := range []string{
		"", "svc_界", "svc_" + strings.Repeat("界", 700),
		strings.Repeat("a", 2_001), strings.Repeat("é", 1_500),
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, body string) {
		before := byteCutMentionTokens(body)
		after := runeCutMentionTokens(body)
		if len(before) != len(after) {
			t.Fatalf("token count differs for %q: %d vs %d", body, len(before), len(after))
		}
		for i := range before {
			if before[i] != after[i] {
				t.Fatalf("token %d differs for %q: %q vs %q", i, body, before[i], after[i])
			}
		}
	})
}
