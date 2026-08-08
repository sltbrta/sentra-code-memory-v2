package companydoc

import (
	"strings"
	"unicode"
)

// BagOfWords hashes tokens into a fixed-dim float32 vector (offline dense).
func BagOfWords(text string, dim int) []float32 {
	if dim <= 0 {
		dim = 64
	}
	v := make([]float32, dim)
	for t := range bagTokens(text) {
		h := 0
		for _, c := range t {
			h = h*31 + int(c)
		}
		if h < 0 {
			h = -h
		}
		v[h%dim] += 1
	}
	return v
}

func bagTokens(s string) map[string]bool {
	out := map[string]bool{}
	var b strings.Builder
	flush := func() {
		if b.Len() >= 3 {
			out[strings.ToLower(b.String())] = true
		}
		b.Reset()
	}
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' {
			b.WriteRune(r)
		} else {
			flush()
		}
	}
	flush()
	return out
}

func tokenOverlap(a, b map[string]bool) float64 {
	if len(a) == 0 {
		return 0
	}
	n := 0
	for t := range a {
		if b[t] {
			n++
		}
	}
	return float64(n) / float64(len(a))
}
