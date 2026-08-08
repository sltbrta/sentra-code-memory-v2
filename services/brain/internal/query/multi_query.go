package query

import (
	"regexp"
	"strings"
	"unicode"
)

// multiQueryMaxVariants caps alternate lexical queries so retrieval stays
// bounded and deterministic.
const multiQueryMaxVariants = 4

// contentWordMinLen is the minimum alphabetic token length retained in the
// content-word short form (stopwords are still dropped regardless of length).
const contentWordMinLen = 4

// Identifier patterns for the enterprise floor: ticket keys (TICKET-123),
// dotted metrics/paths (api.latency.p99), and ALLCAPS codes (HTTP, SLO_P95).
var (
	identTicket  = regexp.MustCompile(`\b([A-Z][A-Z0-9]+-\d+)\b`)
	identDotted  = regexp.MustCompile(`\b([A-Za-z][\w]*\.[A-Za-z][\w\.]{1,60})\b`)
	identAllCaps = regexp.MustCompile(`\b([A-Z]{2,}[A-Z0-9_]*)\b`)
)

// multiQueryStop is a small English interrogative/function-word set used only
// for content-word short forms. It is deliberately smaller than a full NLP
// stop list: rare content tokens must survive.
var multiQueryStop = map[string]bool{
	"what": true, "when": true, "where": true, "which": true, "who": true,
	"whom": true, "whose": true, "why": true, "how": true, "does": true,
	"did": true, "the": true, "and": true, "for": true, "from": true,
	"with": true, "into": true, "about": true, "that": true, "this": true,
	"these": true, "those": true, "are": true, "was": true, "were": true,
	"been": true, "have": true, "has": true, "had": true, "will": true,
	"would": true, "should": true, "could": true, "can": true, "may": true,
	"might": true, "must": true, "not": true, "new": true, "recent": true,
	"current": true, "default": true, "exact": true, "specific": true,
	"using": true, "during": true, "after": true, "before": true,
	"between": true, "through": true, "their": true, "there": true,
	"they": true, "them": true,
}

// multiQueryVariants returns the original question plus deterministic
// lexical alternates: a content-word short form and an identifier-only
// query when high-specificity tokens are present. Results are deduped
// (case-insensitive) and capped at multiQueryMaxVariants.
func multiQueryVariants(question string) []string {
	question = strings.TrimSpace(question)
	if question == "" {
		return nil
	}
	variants := []string{question}
	seen := map[string]bool{strings.ToLower(question): true}

	add := func(v string) {
		v = strings.TrimSpace(v)
		if v == "" {
			return
		}
		key := strings.ToLower(v)
		if seen[key] || len(variants) >= multiQueryMaxVariants {
			return
		}
		seen[key] = true
		variants = append(variants, v)
	}

	if short := contentWordShortForm(question); short != "" {
		add(short)
	}
	if ids := extractQueryIdentifiers(question); len(ids) > 0 {
		add(strings.Join(ids, " "))
	}
	return variants
}

// contentWordShortForm keeps alphabetic tokens of length >= contentWordMinLen
// that are not stopwords, joined in appearance order (capped at 10 tokens).
func contentWordShortForm(question string) string {
	var words []string
	for _, token := range strings.Fields(question) {
		token = strings.Trim(token, tokenCutset)
		if token == "" {
			continue
		}
		// Alphabetic-only content words (drop paths, numbers, punctuation).
		if !isAlphaToken(token) {
			continue
		}
		if len(token) < contentWordMinLen {
			continue
		}
		if multiQueryStop[strings.ToLower(token)] {
			continue
		}
		words = append(words, token)
		if len(words) >= 10 {
			break
		}
	}
	if len(words) == 0 {
		return ""
	}
	return strings.Join(words, " ")
}

func isAlphaToken(token string) bool {
	for _, r := range token {
		if !unicode.IsLetter(r) {
			return false
		}
	}
	return token != ""
}

// extractQueryIdentifiers returns high-specificity tokens: ticket keys,
// dotted metrics, and ALLCAPS codes, longest-first, capped at 6.
func extractQueryIdentifiers(question string) []string {
	type hit struct {
		text string
		pos  int
	}
	var hits []hit
	seen := map[string]bool{}
	addMatch := func(raw string, pos int) {
		raw = strings.TrimSpace(raw)
		if len(raw) < 2 {
			return
		}
		key := strings.ToLower(raw)
		if seen[key] || multiQueryStop[key] {
			return
		}
		seen[key] = true
		hits = append(hits, hit{text: raw, pos: pos})
	}
	for _, re := range []*regexp.Regexp{identTicket, identDotted, identAllCaps} {
		for _, loc := range re.FindAllStringSubmatchIndex(question, -1) {
			if len(loc) < 4 {
				continue
			}
			addMatch(question[loc[2]:loc[3]], loc[2])
		}
	}
	// Longest first (identifier floor preference), then appearance order.
	for i := 0; i < len(hits); i++ {
		for j := i + 1; j < len(hits); j++ {
			if len(hits[j].text) > len(hits[i].text) ||
				(len(hits[j].text) == len(hits[i].text) && hits[j].pos < hits[i].pos) {
				hits[i], hits[j] = hits[j], hits[i]
			}
		}
	}
	out := make([]string, 0, len(hits))
	for _, h := range hits {
		out = append(out, h.text)
		if len(out) >= 6 {
			break
		}
	}
	return out
}
