package codecrawl

import (
	"math"
	"path/filepath"
	"sort"
	"strings"
)

// Search ranks indexed files by query-token term frequency sum.
func (idx *Index) Search(query string, topK int) []Hit {
	return idx.SearchOpts(query, topK, false)
}

// SearchOpts is multi-arm code search: lexical TF + code-rank heuristics +
// optional symbol hop (stack-graph virtual cross-file edges over shared names).
//
// Ranking:
//   - log1p(tf) dampens mega-fixture TF floods
//   - path demotion: tests/fixtures/perf + vendored copilot mirrors
//   - path promotion: src/ production paths, shallower depth for same stem
//   - exact file stem match >> compound *Name* filenames
//   - def boost when file defines a query-token symbol
//
// boostInterfaceQuery raises scores when query looks like IFooService-style names.
func boostInterfaceQuery(query string, path string, score float64) float64 {
	q := strings.ToLower(strings.TrimSpace(query))
	if len(q) < 3 {
		return score
	}
	// Strip leading I for interface tokens (TypeScript/C# style).
	stem := q
	if len(stem) > 2 && stem[0] == 'i' && stem[1] >= 'a' && stem[1] <= 'z' {
		// only if camel-ish after I
		if len(query) > 1 && query[0] == 'I' && query[1] >= 'A' && query[1] <= 'Z' {
			stem = strings.ToLower(query[1:])
		}
	}
	pl := strings.ToLower(filepath.ToSlash(path))
	base := filepath.Base(pl)
	if strings.Contains(base, stem) || strings.Contains(pl, stem) {
		return score * 1.8
	}
	// Query "IConfigurationService" vs path configurationService.ts
	compact := strings.ReplaceAll(stem, "service", "")
	if len(compact) >= 4 && strings.Contains(base, compact) {
		return score * 1.5
	}
	return score
}

func (idx *Index) SearchOpts(query string, topK int, symbolHop bool) []Hit {
	if idx == nil || len(idx.inverted) == 0 {
		return nil
	}
	qTokens := queryTokens(query)
	if len(qTokens) == 0 {
		return nil
	}

	seen := make(map[string]struct{}, len(qTokens))
	var uniq []string
	scores := make(map[string]float64)
	for _, t := range qTokens {
		if _, dup := seen[t]; dup {
			continue
		}
		seen[t] = struct{}{}
		uniq = append(uniq, t)
		postings, ok := idx.inverted[t]
		if !ok {
			continue
		}
		docFreq := len(postings)
		idf := math.Log1p(float64(maxInt(idx.FileCount(), 1)) / float64(docFreq+1))
		for path, tf := range postings {
			scores[path] += math.Log1p(float64(tf)) * idf
		}
	}
	if len(scores) == 0 {
		return nil
	}

	defFiles := map[string]struct{}{}
	if idx.symbols != nil {
		for _, t := range uniq {
			for _, f := range idx.symbols.Defs[t] {
				defFiles[f] = struct{}{}
			}
		}
	}
	for path, defs := range idx.fileDefs {
		for _, d := range defs {
			dl := strings.ToLower(d)
			for _, t := range uniq {
				if dl == t {
					defFiles[path] = struct{}{}
				}
			}
		}
	}

	hits := make([]Hit, 0, len(scores))
	for path, score := range scores {
		matched := 0
		for _, t := range uniq {
			if _, ok := idx.inverted[t][path]; ok {
				matched++
			}
		}
		if len(uniq) > 1 {
			coverage := float64(matched) / float64(len(uniq))
			// A repeated generic token must not outrank a file that covers the
			// whole request. Keep partial matches visible, but demote them.
			score *= 0.10 + 0.90*coverage
		}
		s := score * pathRankMultiplier(path)
		base := strings.ToLower(filepath.Base(path))
		rawStem := strings.TrimSuffix(base, filepath.Ext(base))
		stem := normalizeStem(rawStem)
		pl := strings.ToLower(filepath.ToSlash(path))
		for _, t := range uniq {
			if t == "" {
				continue
			}
			// Exact file stem (textmodel.ts) beats compound names.
			if stem == t || rawStem == t || base == t {
				s += 28
			} else if strings.HasPrefix(stem, t) || strings.HasSuffix(stem, t) {
				s += 10
			} else if strings.Contains(stem, t) || strings.Contains(base, t) {
				s += 3
			}
			if strings.Contains(pl, "/"+t+".") || strings.Contains(pl, "/"+t+"/") {
				s += 8
			} else if strings.Contains(pl, t) {
				s += 1
			}
		}
		if _, ok := defFiles[path]; ok {
			s += 14
		}
		// Prefer shallower production paths when scores are close.
		s += pathDepthBonus(pl)
		s = boostInterfaceQuery(query, path, s)
		hits = append(hits, Hit{Path: path, Score: s})
	}
	sort.SliceStable(hits, func(i, j int) bool {
		if hits[i].Score != hits[j].Score {
			return hits[i].Score > hits[j].Score
		}
		// Prefer shorter path / src over extensions on ties.
		di, dj := pathDepth(hits[i].Path), pathDepth(hits[j].Path)
		if di != dj {
			return di < dj
		}
		return hits[i].Path < hits[j].Path
	})
	if topK <= 0 || topK > len(hits) {
		topK = len(hits)
	}
	hits = hits[:topK]

	if !symbolHop || idx.symbols == nil {
		return hits
	}
	seeds := make([]string, len(hits))
	for i, h := range hits {
		seeds[i] = h.Path
	}
	neighbors := idx.SymbolHop(seeds, topK)
	if len(neighbors) == 0 {
		return hits
	}
	have := map[string]struct{}{}
	for _, h := range hits {
		have[h.Path] = struct{}{}
	}
	for _, n := range neighbors {
		if _, ok := have[n]; ok {
			continue
		}
		hits = append(hits, Hit{Path: n, Score: 0.5 * pathRankMultiplier(n)})
		have[n] = struct{}{}
		if topK > 0 && len(hits) >= topK*2 {
			break
		}
	}
	return hits
}

var queryStopWords = map[string]struct{}{
	"a": {}, "an": {}, "and": {}, "are": {}, "as": {}, "at": {}, "be": {},
	"by": {}, "for": {}, "from": {}, "how": {}, "in": {}, "is": {}, "it": {},
	"of": {}, "on": {}, "or": {}, "the": {}, "to": {}, "what": {}, "where": {},
	"which": {}, "with": {}, "why": {},
}

func queryTokens(query string) []string {
	tokens := tokenize(query)
	if len(tokens) <= 1 {
		return tokens
	}
	filtered := make([]string, 0, len(tokens))
	for _, token := range tokens {
		if _, stop := queryStopWords[token]; !stop {
			filtered = append(filtered, token)
		}
	}
	if len(filtered) == 0 {
		return tokens
	}
	return filtered
}

func maxInt(value, minimum int) int {
	if value < minimum {
		return minimum
	}
	return value
}

func normalizeStem(s string) string {
	s = strings.ToLower(s)
	s = strings.ReplaceAll(s, ".", "")
	s = strings.ReplaceAll(s, "-", "")
	s = strings.ReplaceAll(s, "_", "")
	return s
}

func pathDepth(path string) int {
	return strings.Count(filepath.ToSlash(path), "/")
}

func pathDepthBonus(pl string) float64 {
	// Prefer src/vs production over extensions/ mirrors.
	if strings.HasPrefix(pl, "src/") {
		return 3
	}
	if strings.Contains(pl, "/src/") {
		return 1.5
	}
	return 0
}

// pathRankMultiplier demotes tests/fixtures/vendored mirrors that flood TF.
func pathRankMultiplier(path string) float64 {
	p := strings.ToLower(filepath.ToSlash(path))
	mult := 1.0
	switch {
	case strings.Contains(p, "/test/"),
		strings.Contains(p, "/tests/"),
		strings.Contains(p, "/__tests__/"),
		strings.Contains(p, ".test."),
		strings.Contains(p, ".spec."),
		strings.Contains(p, "_test."),
		strings.HasSuffix(p, "_test.go"),
		strings.Contains(p, "/fixtures/"),
		strings.Contains(p, "/fixture/"),
		strings.Contains(p, "perf-data"),
		strings.Contains(p, ".snap"),
		strings.Contains(p, "/mocks/"),
		strings.Contains(p, "/__mocks__/"):
		mult = 0.10
	case strings.Contains(p, "/test"),
		strings.Contains(p, "testing"):
		mult = 0.30
	}
	// Vendored / product-extension mirrors of core (common in VS Code monorepo).
	if strings.Contains(p, "extensions/copilot/") ||
		strings.Contains(p, "/util/vs/") ||
		strings.Contains(p, "node_modules/") {
		mult *= 0.25
	}
	if strings.HasPrefix(p, "src/") {
		mult *= 1.15
	}
	// Prefer service/interface-looking files when path contains common stems.
	base := filepath.Base(p)
	if strings.HasPrefix(base, "i") && strings.Contains(base, "service") {
		mult *= 1.25
	}
	if strings.Contains(base, "configuration") || strings.Contains(base, "configservice") {
		mult *= 1.2
	}
	return mult
}

// SymbolStats returns coarse counts for diagnostics (defs/refs).
func (idx *Index) SymbolStats() (defs, refs int) {
	if idx == nil || idx.symbols == nil {
		return 0, 0
	}
	return len(idx.symbols.Defs), len(idx.symbols.Refs)
}
