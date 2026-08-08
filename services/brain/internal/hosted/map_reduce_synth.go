package hosted

import (
	"context"
	"fmt"
	"strings"
	"sync"
)

// mapReduceSynthesize answers multi-doc project/completeness questions by
// extracting per-facet notes then reducing — closes project_related combined~12
// gap where single-shot Flash missed multi-gold facts despite pool hits.
//
// Map: short extractive/LLM note per top facet (entity/source).
// Reduce: one synthesizeOnce over concatenated notes + original pack head.
func (c *Client) mapReduceSynthesize(
	ctx context.Context,
	question, questionType string,
	passages []Passage,
	maxChars int,
	sourceTypes []string,
	history string,
) (synthRaw, string, string, error) {
	if c == nil || len(passages) == 0 {
		return synthRaw{}, "", "", fmt.Errorf("map_reduce: empty")
	}
	// Deadline-aware (issue #278): map-reduce costs up to facets+1 generation
	// calls — when the request deadline can't fit them, fall back to the
	// single-shot path instead of starting a serial runaway.
	if !deadlineMarginOK(ctx) {
		ledgerFrom(ctx).skip("map_reduce", "deadline_margin")
		ledgerFrom(ctx).beginCall("synth", "primary_after_map_reduce_skip")
		return synthesizeOnce(ctx, question, questionType, passages, maxChars, "", sourceTypes, history)
	}
	facets := exhaustiveFacets(c, ctx, question, passages, mapReduceMaxFacets())
	if len(facets) == 0 {
		// Fall back to single-shot.
		ledgerFrom(ctx).beginCall("synth", "primary_after_map_reduce_no_facets")
		return synthesizeOnce(ctx, question, questionType, passages, maxChars, "", sourceTypes, history)
	}

	type note struct {
		facet string
		text  string
		cites []string
	}
	notes := make([]note, len(facets))
	var wg sync.WaitGroup
	for i, f := range facets {
		wg.Add(1)
		go func(i int, facet string) {
			defer wg.Done()
			// Local pack: passages overlapping this facet.
			var sub []Passage
			fl := strings.ToLower(facet)
			for _, p := range passages {
				if strings.Contains(strings.ToLower(p.Text), fl) ||
					strings.Contains(strings.ToLower(p.DocumentID), fl) {
					sub = append(sub, p)
				}
			}
			if len(sub) == 0 {
				sub = passages
				if len(sub) > 4 {
					sub = sub[:4]
				}
			} else if len(sub) > 4 {
				sub = sub[:4]
			}
			q := "Extract every concrete fact from the documents that helps answer: " + question +
				" Focus on: " + facet + ". Be terse; include numbers/names/dates verbatim."
			ledgerFrom(ctx).beginCall("map_reduce_map", "facet:"+facet)
			raw, _, _, err := synthesizeOnce(withLLMStage(ctx, "map_reduce_map"), q, "basic", sub, maxChars/2, "", sourceTypes, "")
			if err != nil || strings.TrimSpace(raw.Answer) == "" {
				// Extractive fallback per facet.
				notes[i] = note{
					facet: facet,
					text:  extractiveForQuestion(question+" "+facet, sub),
					cites: passageIDs(sub),
				}
				return
			}
			notes[i] = note{facet: facet, text: raw.Answer, cites: raw.Cited}
		}(i, f)
	}
	wg.Wait()

	var b strings.Builder
	b.WriteString("Facet notes (map stage) — synthesize a single complete answer to the user question.\n")
	b.WriteString("Question: ")
	b.WriteString(question)
	b.WriteString("\n\n")
	var allCites []string
	for _, n := range notes {
		if strings.TrimSpace(n.text) == "" {
			continue
		}
		b.WriteString("### ")
		b.WriteString(n.facet)
		b.WriteString("\n")
		b.WriteString(n.text)
		b.WriteString("\n\n")
		allCites = append(allCites, n.cites...)
	}
	// Include top pack passages as evidence for reduce.
	reducePack := passages
	if len(reducePack) > 10 {
		reducePack = reducePack[:10]
	}
	suffix := "\n\n" + b.String() +
		"\n\nIMPORTANT: Merge all facet notes into one coherent answer. " +
		"Enumerate every distinct fact; cite multiple document ids when facts span sources. " +
		"Do not drop owners, dates, SLOs, or thresholds present in any note."
	ledgerFrom(ctx).beginCall("map_reduce_reduce", "merge_facet_notes")
	raw, prov, model, err := synthesizeOnce(withLLMStage(ctx, "map_reduce_reduce"), question, questionType, reducePack, maxChars, suffix, sourceTypes, history)
	if err != nil {
		return raw, prov, model, err
	}
	// Prefer multi-cite from map stage if reduce under-cited.
	if len(raw.Cited) < 2 && len(allCites) > 0 {
		raw.Cited = uniqueStringsStable(append(raw.Cited, allCites...))
		if len(raw.Cited) > 10 {
			raw.Cited = raw.Cited[:10]
		}
	}
	return raw, prov, model, nil
}

// wantsMapReduceSynth: multi-doc project/completeness under QUALITY budgets.
func wantsMapReduceSynth(questionType string, plan QueryPlan) bool {
	if !envTruthy("OUROBOROS_ERB_QUALITY", false) &&
		!benchmaxEnabled() &&
		!strings.EqualFold(envOr("OUROBOROS_ERB_MODE", ""), "bench") &&
		!strings.EqualFold(envOr("OUROBOROS_ERB_MODE", ""), "research") {
		// Opt-in on light via env.
		if !envTruthy("OUROBOROS_ERB_MAP_REDUCE", false) {
			return false
		}
	}
	qt := strings.ToLower(strings.TrimSpace(questionType))
	if qt == "project_related" || qt == "completeness" {
		return true
	}
	if plan.Completeness {
		return true
	}
	return false
}
