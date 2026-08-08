package hosted

import (
	"context"
	"strings"
	"sync"
)

// exhaustiveMapReduceExpand is the product analogue of sentra's aggregation
// agentic path — without a separate research-only fork.
//
// When the question is aggregation/completeness-shaped (or CE signal is weak
// with multi-doc plan):
//  1. Map: fan out retrieve on entity/source facets mined from the seed pack
//  2. Reduce: union unique docs into the window (cap maxExtra)
//
// This is pure retrieve expand (no multi-LLM map-reduce cost on light). Synth
// still uses completeness retry + pack discipline for enumeration.
func (c *Client) exhaustiveMapReduceExpand(
	ctx context.Context,
	question, questionType string,
	window []Passage,
	maxExtra int,
	sourceTypes []string,
	filter map[string]any,
) ([]Passage, map[string]any) {
	diag := map[string]any{
		"exhaustive_agentic": false,
		"exhaustive_map_n":   0,
		"exhaustive_added":   0,
	}
	if c == nil || maxExtra < 1 {
		return window, diag
	}
	ledger := retrievalExpansionLedgerFrom(ctx)
	if ledger == nil {
		ledger = newRequestRetrievalExpansionLedger()
		ctx = withRetrievalExpansionLedger(ctx, ledger)
	}
	ctx, budgetCancel := ledger.budgetContext(ctx)
	defer budgetCancel()
	if !ledger.canContinue(ctx, "exhaustive_map") {
		ledger.stampInto(diag)
		return window, diag
	}
	// Facets: identifiers + entity catalog + pack-mined proper nouns.
	// Cap 4 ExpandLite maps — full RetrieveOpts ×8 was the 200s answer_total sink.
	facets := exhaustiveFacets(c, ctx, question, window, 4)
	if len(facets) == 0 {
		return window, diag
	}
	diag["exhaustive_map_n"] = len(facets)
	diag["exhaustive_facets"] = facets

	type pack struct {
		ps []Passage
	}
	ch := make(chan pack, len(facets))
	var wg sync.WaitGroup
	for _, f := range facets {
		wg.Add(1)
		go func(facet string) {
			defer wg.Done()
			// ExpandLite only: HotLex+1 dense, no recovery/structure/grep stack.
			q := facet + " " + compactQuestionBag(question, 8)
			more, _, err, attempted := c.expansionRetrieve(
				ctx, "exhaustive_map", q, 6, "basic", sourceTypes, filter,
			)
			if !attempted || err != nil || len(more) == 0 {
				ch <- pack{}
				return
			}
			ch <- pack{ps: more}
		}(f)
	}
	go func() {
		wg.Wait()
		close(ch)
	}()

	cur := append([]Passage(nil), window...)
	before := len(uniqueDSIDs(cur))
	for p := range ch {
		if len(p.ps) == 0 {
			continue
		}
		cur = mergePassages(cur, p.ps, len(cur)+maxExtra+4)
	}
	// Cap growth.
	if maxExtra > 0 {
		want := before + maxExtra
		ids := uniqueDSIDs(cur)
		if len(ids) > want {
			// Keep original window order first, then extras.
			keep := map[string]struct{}{}
			for _, id := range uniqueDSIDs(window) {
				keep[id] = struct{}{}
			}
			var out []Passage
			seen := map[string]struct{}{}
			for _, p := range window {
				if _, ok := seen[p.DocumentID]; ok {
					continue
				}
				seen[p.DocumentID] = struct{}{}
				out = append(out, p)
			}
			for _, p := range cur {
				if _, ok := seen[p.DocumentID]; ok {
					continue
				}
				if len(uniqueDSIDs(out)) >= want {
					break
				}
				seen[p.DocumentID] = struct{}{}
				out = append(out, p)
			}
			cur = out
		}
	}
	added := len(uniqueDSIDs(cur)) - before
	if added < 0 {
		added = 0
	}
	diag["exhaustive_agentic"] = true
	diag["exhaustive_added"] = added
	ledger.stampInto(diag)
	return cur, diag
}

func exhaustiveFacets(c *Client, ctx context.Context, question string, window []Passage, maxN int) []string {
	if maxN <= 0 {
		maxN = 8
	}
	seen := map[string]struct{}{}
	var out []string
	add := func(s string) {
		s = strings.TrimSpace(s)
		if len(s) < 3 || len(s) > 80 {
			return
		}
		// Skip pure stop / tiny.
		k := strings.ToLower(s)
		if _, ok := seen[k]; ok {
			return
		}
		seen[k] = struct{}{}
		out = append(out, s)
	}
	for _, id := range extractIdentifiers(question) {
		add(id)
	}
	if c != nil {
		for _, t := range c.entityCatalogTerms(ctx, question, maxN) {
			add(t)
		}
	}
	// Mine capitalised / title-ish tokens from pack heads (customers, projects).
	for _, p := range window {
		if len(out) >= maxN {
			break
		}
		head := bodyHead(p.Text, 200)
		for _, w := range strings.Fields(head) {
			if len(w) >= 4 && w[0] >= 'A' && w[0] <= 'Z' {
				add(strings.Trim(w, ".,;:()[]\""))
			}
		}
	}
	// Source-type facets for completeness (per-channel recall).
	ql := strings.ToLower(question)
	if strings.Contains(ql, "customer") || strings.Contains(ql, "channel") ||
		strings.Contains(ql, "every") || strings.Contains(ql, "all ") {
		for _, src := range []string{"confluence", "slack", "jira", "github", "linear", "notion"} {
			if strings.Contains(ql, src) {
				add(src + " " + compactQuestionBag(question, 6))
			}
		}
	}
	if len(out) > maxN {
		out = out[:maxN]
	}
	return out
}

// wantsExhaustiveAgentic: aggregation / completeness / multi-customer surface.
func wantsExhaustiveAgentic(question, questionType string, plan QueryPlan) bool {
	if plan.Completeness || strings.EqualFold(questionType, "completeness") {
		return true
	}
	if aggQuestionRE.MatchString(question) {
		return true
	}
	ql := strings.ToLower(question)
	for _, c := range []string{
		"list all", "list every", "enumerate", "across all", "company-wide",
		"which customers", "every customer", "all channels", "complete list",
	} {
		if strings.Contains(ql, c) {
			return true
		}
	}
	return false
}
