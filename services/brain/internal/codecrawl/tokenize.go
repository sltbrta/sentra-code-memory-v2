package codecrawl

import (
	"strings"
	"unicode"
)

// tokenize splits s into lowercased alphanumeric tokens (letter/number runs).
// Empty or non-alphanumeric input yields a nil slice.
func tokenize(s string) []string {
	fields := strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsNumber(r)
	})
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		if f == "" {
			continue
		}
		out = append(out, f)
	}
	return out
}

// tokenFreq counts term frequency for tokens in s (path and content callers).
func tokenFreq(s string) map[string]int {
	tokens := tokenize(s)
	if len(tokens) == 0 {
		return nil
	}
	freq := make(map[string]int, len(tokens)/2+1)
	for _, t := range tokens {
		freq[t]++
	}
	return freq
}
