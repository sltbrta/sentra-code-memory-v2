package hosted

import (
	"context"
	"os"
	"strings"
)

// decomposeQuery splits multi-part questions into sub-queries for union retrieve.
// Deterministic heuristics (no LLM) for product offline tests + residual parity.
func decomposeQuery(question, questionType string) []string {
	q := strings.TrimSpace(question)
	if q == "" {
		return nil
	}
	// Always include original first (caller may already have multiQueryVariants).
	subs := []string{}

	// Split on question marks if multiple
	parts := strings.Split(q, "?")
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if len(p) < 12 {
			continue
		}
		if !strings.HasSuffix(p, "?") {
			p = p + "?"
		}
		subs = append(subs, p)
	}

	// Conjunction splits for multi-gold types (Proxima: cause + exception + verify SLO).
	qt := strings.ToLower(questionType)
	if qt == "project_related" || qt == "completeness" || qt == "conflicting_info" || qt == "semantic" || qt == "constrained" {
		for _, sep := range []string{
			" and what ", " and how ", " and why ",
			" and ", " as well as ", "; ", " also ",
		} {
			low := strings.ToLower(q)
			idx := strings.Index(low, sep)
			if idx < 0 {
				continue
			}
			a := strings.TrimSpace(q[:idx])
			b := strings.TrimSpace(q[idx+len(sep):])
			if len(a) > 15 {
				subs = append(subs, a)
			}
			if len(b) > 15 {
				// Restore leading cue stripped by sep for "what"/"how" splits.
				cue := strings.TrimSpace(sep)
				if strings.HasPrefix(cue, "and ") {
					b = strings.TrimPrefix(cue, "and ") + " " + b
				}
				subs = append(subs, b)
			}
		}
	}

	// "what X and what Y" style
	low := strings.ToLower(q)
	if strings.Count(low, "what ") >= 2 {
		// leave subs from ? split
	}

	return dedupeQueries(subs)
}

// hydeStub builds a weak hypothetical document string for dense expand without LLM.
// Real HyDE uses LLM; this deterministic paraphrase still diversifies dense queries.
func hydeStub(question string) string {
	q := strings.TrimSpace(question)
	if q == "" {
		return ""
	}
	// Convert question words to declarative-ish bag
	toks := contentTokens(q)
	if len(toks) == 0 {
		return "Document discussing: " + q
	}
	n := len(toks)
	if n > 12 {
		n = 12
	}
	return "This document states details about " + strings.Join(toks[:n], " ") +
		". It includes definitions, owners, thresholds, and procedures."
}

// hydeDocument returns a hypothetical-document string for dense expand.
// Hermetic default: always hydeStub (no network). Opt-in LLM via
// OUROBOROS_ERB_HYDE_FORCE_LLM=1 and OPENAI_API_KEY (quality path only).
func hydeDocument(ctx context.Context, question string, quality bool) (string, string) {
	if !quality {
		return hydeStub(question), "stub"
	}
	if !envTruthy("OUROBOROS_ERB_HYDE_FORCE_LLM", false) {
		return hydeStub(question), "stub_quality"
	}
	key := strings.TrimSpace(os.Getenv("OPENAI_API_KEY"))
	if key == "" {
		return hydeStub(question), "stub_no_key"
	}
	// Real LLM HyDE (optional; not used in unit/CI hermetic defaults).
	raw, _, _, err := synthesizeOnce(withLLMStage(ctx, "hyde_expand"), "Write a short paragraph that would answer: "+question,
		"basic", nil, 0, "\n\nReturn JSON {\"answer\":\"...\"} only.", nil, "")
	if err != nil || strings.TrimSpace(raw.Answer) == "" {
		return hydeStub(question), "stub_llm_fallback"
	}
	_ = key
	return raw.Answer, "llm"
}

// qualityDoc2QueryVariants emits extra search questions when quality mode is on.
// Deterministic (no LLM) so unit tests stay hermetic; LLM path is gardener-time.
func qualityDoc2QueryVariants(question string) []string {
	q := strings.TrimSpace(question)
	if q == "" {
		return nil
	}
	ids := extractIdentifiers(q)
	toks := contentTokens(q)
	var out []string
	if len(ids) > 0 {
		out = append(out, "definition of "+ids[0])
		out = append(out, ids[0]+" policy requirements")
		out = append(out, ids[0]+" threshold configuration")
	}
	if len(toks) >= 3 {
		out = append(out, strings.Join(toks[:min(5, len(toks))], " ")+" procedure")
		out = append(out, strings.Join(toks[:min(6, len(toks))], " ")+" default")
	}
	// Long paraphrase: a noun-heavy bag gives ANN/FTS a compact alternative
	// when the original question has little surface overlap with the corpus.
	if len(toks) >= 8 {
		start := 0
		for start < len(toks) && start < 3 {
			switch toks[start] {
			case "what", "when", "where", "which", "how", "during", "recent":
				start++
			default:
				break
			}
			if start < len(toks) && start < 3 {
				t := toks[start]
				if t != "what" && t != "when" && t != "where" && t != "which" && t != "how" && t != "during" && t != "recent" {
					break
				}
			}
		}
		end := start + 8
		if end > len(toks) {
			end = len(toks)
		}
		out = append(out, strings.Join(toks[start:end], " "))
	}
	out = append(out, pickHotLexPhrases(q, 3)...)
	return dedupeQueries(out)
}

// phraseHopQueries builds phrase/AND style rescue queries (quality second-hop).
func phraseHopQueries(question string, passages []Passage) []string {
	ids := extractIdentifiers(question)
	var out []string
	if len(ids) >= 2 {
		// AND-style bag
		n := len(ids)
		if n > 4 {
			n = 4
		}
		out = append(out, strings.Join(ids[:n], " "))
		out = append(out, `"`+ids[0]+`"`)
	}
	// Entity-ish tokens from top passages
	for i, p := range passages {
		if i >= 3 {
			break
		}
		for _, id := range extractIdentifiers(p.Text) {
			if len(id) >= 4 {
				out = append(out, id)
			}
			if len(out) >= 6 {
				break
			}
		}
	}
	return dedupeQueries(out)
}
