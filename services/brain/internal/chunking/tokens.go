package chunking

import (
	"strings"
	"unicode"
)

// TokenizerID pins the token model used for sizing and overlap. Version 1
// counts whitespace-separated words with byte offsets into the source
// string. It is a deliberate, documented approximation: swapping to a model
// tokenizer is a tokenizer version bump AND a policy version bump, because
// chunk boundaries (and therefore chunk identity) change.
const TokenizerID = "ouroboros-ws-1"

// Token is one token with byte offsets into the source string.
type Token struct {
	Text  string
	Start int // byte offset, inclusive
	End   int // byte offset, exclusive
}

// Tokenize splits s into maximal non-space runs with byte offsets.
// Deterministic: same input always yields the same tokens.
func Tokenize(s string) []Token {
	var toks []Token
	start := -1
	for i, r := range s {
		if unicode.IsSpace(r) {
			if start >= 0 {
				toks = append(toks, Token{Text: s[start:i], Start: start, End: i})
				start = -1
			}
			continue
		}
		if start < 0 {
			start = i
		}
	}
	if start >= 0 {
		toks = append(toks, Token{Text: s[start:], Start: start, End: len(s)})
	}
	return toks
}

// CountTokens counts tokens without retaining them.
func CountTokens(s string) int {
	return len(strings.Fields(s))
}
