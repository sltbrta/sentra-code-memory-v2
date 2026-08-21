package memory

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"time"

	"github.com/sltbrta/sentra-code-memory-v2/services/internal/textbound"
)

// LLMExtractFunc is injected by residual to avoid memory→hosted import cycles.
// Returns raw model text; OpenIELLMParse extracts SPO triples.
type LLMExtractFunc func(ctx context.Context, system, user string) (string, error)

// OpenIELLMEnabled is true when OUROBOROS_BRAIN_OPENIE_LLM=1.
func OpenIELLMEnabled() bool {
	v := strings.TrimSpace(os.Getenv("OUROBOROS_BRAIN_OPENIE_LLM"))
	return v == "1" || strings.EqualFold(v, "true") || strings.EqualFold(v, "yes")
}

// ExtractClaimsOpenIELLM asks an LLM for JSON triples, fail-closed to empty.
// Provenance: openie_llm. Caller must provide a working LLMExtractFunc.
func ExtractClaimsOpenIELLM(ctx context.Context, docID, text string, llm LLMExtractFunc) []Claim {
	if llm == nil || docID == "" || strings.TrimSpace(text) == "" {
		return nil
	}
	text = textbound.Bytes(text, 6000)
	sys := `Extract factual subject-predicate-object triples from the document.
Reply with ONLY a JSON array: [{"subject":"...","predicate":"...","object":"...","span":"..."}]
Max 12 triples. High precision only. No prose.`
	user := "Document id=" + docID + "\n\n" + text
	raw, err := llm(ctx, sys, user)
	if err != nil || strings.TrimSpace(raw) == "" {
		return nil
	}
	return ParseOpenIEJSON(docID, raw)
}

// ParseOpenIEJSON parses model output into Claims with provenance openie_llm.
func ParseOpenIEJSON(docID, raw string) []Claim {
	raw = strings.TrimSpace(raw)
	// Strip markdown fences if present.
	if i := strings.Index(raw, "["); i >= 0 {
		if j := strings.LastIndex(raw, "]"); j > i {
			raw = raw[i : j+1]
		}
	}
	var rows []struct {
		Subject   string `json:"subject"`
		Predicate string `json:"predicate"`
		Object    string `json:"object"`
		Span      string `json:"span"`
	}
	if err := json.Unmarshal([]byte(raw), &rows); err != nil {
		return nil
	}
	now := time.Now().UTC()
	var out []Claim
	seen := map[string]struct{}{}
	for _, r := range rows {
		sub := normalizeClaimPart(r.Subject)
		pred := normalizeClaimPart(r.Predicate)
		obj := normalizeClaimPart(r.Object)
		if sub == "" || pred == "" || obj == "" {
			continue
		}
		if !looksLikeEntity(sub) {
			continue
		}
		key := strings.ToLower(sub + "|" + pred + "|" + obj)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		span := strings.TrimSpace(r.Span)
		if span == "" {
			// Synthesize a span from SPO when model omits it (GAP-MEM-EVIDENCE-SPANS).
			span = sub + " " + pred + " " + obj
		}
		cl := Claim{
			Subject: sub, Predicate: pred, Object: obj,
			DocumentIDs: []string{docID},
			ValidFrom:   now, ObservedAt: now, Status: ClaimActive,
			Provenance: "openie_llm", SpanDocID: docID, SpanText: span,
		}
		out = append(out, cl)
		if len(out) >= 12 {
			break
		}
	}
	return out
}

// FillClaimSpanOffsets resolves SpanStart/SpanEnd from SpanText within docText.
// Call after LLM extract when only SpanText is present (GAP-MEM-EVIDENCE-SPANS).
func FillClaimSpanOffsets(cl *Claim, docText string) {
	if cl == nil || cl.SpanText == "" || docText == "" {
		return
	}
	if cl.SpanStart > 0 || cl.SpanEnd > cl.SpanStart {
		return // already set
	}
	idx := strings.Index(strings.ToLower(docText), strings.ToLower(cl.SpanText))
	if idx < 0 {
		// Fallback: try subject alone.
		if cl.Subject != "" {
			idx = strings.Index(strings.ToLower(docText), strings.ToLower(cl.Subject))
		}
	}
	if idx < 0 {
		return
	}
	cl.SpanStart = idx
	end := idx + len(cl.SpanText)
	if end > len(docText) {
		end = len(docText)
	}
	cl.SpanEnd = end
	if cl.SpanDocID == "" && len(cl.DocumentIDs) > 0 {
		cl.SpanDocID = cl.DocumentIDs[0]
	}
}
