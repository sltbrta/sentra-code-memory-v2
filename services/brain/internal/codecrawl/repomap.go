package codecrawl

import (
	"math"
	"sort"
	"strings"
)

// RepoMapOptions bounds task-personalized file/symbol ranking.
type RepoMapOptions struct {
	MaxFiles   int
	MaxSymbols int
	Iterations int
}

// RankedSymbol is one symbol ranked from the personalized file graph.
type RankedSymbol struct {
	Name  string  `json:"name"`
	Score float64 `json:"score"`
	Rank  int     `json:"rank"`
	Kind  string  `json:"kind"`
}

// RepoMapEntry is one bounded file lane in an Aider-style repository map.
type RepoMapEntry struct {
	Path    string         `json:"path"`
	Score   float64        `json:"score"`
	Rank    int            `json:"rank"`
	Direct  bool           `json:"direct,omitempty"`
	Symbols []RankedSymbol `json:"symbols,omitempty"`
}

// RepoMap computes deterministic personalized PageRank over file-local symbol
// links. Query hits seed personalization; shared definitions/references and
// typed edge targets form links. It is heuristic authority, not compiler truth.
func (idx *Index) RepoMap(query string, opts RepoMapOptions) []RepoMapEntry {
	if idx == nil {
		return nil
	}
	if opts.MaxFiles <= 0 {
		opts.MaxFiles = 32
	}
	if opts.MaxFiles > 128 {
		opts.MaxFiles = 128
	}
	if opts.MaxSymbols <= 0 {
		opts.MaxSymbols = 12
	}
	if opts.MaxSymbols > 64 {
		opts.MaxSymbols = 64
	}
	if opts.Iterations <= 0 {
		opts.Iterations = 8
	}
	if opts.Iterations > 32 {
		opts.Iterations = 32
	}
	files := idx.Files()
	sort.Strings(files)
	if len(files) == 0 {
		return nil
	}
	fileSet := map[string]struct{}{}
	for _, f := range files {
		fileSet[f] = struct{}{}
	}
	links := map[string]map[string]struct{}{}
	add := func(a, b string) {
		if a == b {
			return
		}
		if _, ok := fileSet[a]; !ok {
			return
		}
		if _, ok := fileSet[b]; !ok {
			return
		}
		if links[a] == nil {
			links[a] = map[string]struct{}{}
		}
		links[a][b] = struct{}{}
	}
	if idx.symbols != nil {
		for name, defs := range idx.symbols.Defs {
			refs := idx.symbols.Refs[name]
			for _, d := range defs {
				for _, r := range refs {
					add(d, r)
					add(r, d)
				}
			}
		}
	}
	if g := idx.Graph(); g != nil {
		for from, edges := range g.Edges {
			for _, e := range edges {
				if e.Target != "" {
					add(from, e.Target)
				}
			}
		}
	}
	hits := idx.SearchOpts(query, minInt(len(files), opts.MaxFiles*2), true)
	personal := map[string]float64{}
	direct := map[string]struct{}{}
	if len(hits) > 0 {
		total := 0.0
		for i, h := range hits {
			w := h.Score
			if w <= 0 {
				w = 1
			}
			personal[h.Path] += w
			total += w
			if i < maxInt2(1, opts.MaxFiles/4) {
				direct[h.Path] = struct{}{}
			}
		}
		for p := range personal {
			personal[p] /= total
		}
	} else {
		for _, f := range files {
			personal[f] = 1 / float64(len(files))
		}
	}
	rank := map[string]float64{}
	for _, f := range files {
		rank[f] = 1 / float64(len(files))
	}
	for n := 0; n < opts.Iterations; n++ {
		next := map[string]float64{}
		for _, f := range files {
			next[f] = 0.15 * personal[f]
		}
		dangling := 0.0
		for _, f := range files {
			if len(links[f]) == 0 {
				dangling += rank[f]
				continue
			}
			share := 0.85 * rank[f] / float64(len(links[f]))
			for to := range links[f] {
				next[to] += share
			}
		}
		for _, f := range files {
			next[f] += 0.85 * dangling * personal[f]
		}
		rank = next
	}
	entries := make([]RepoMapEntry, 0, len(files))
	for _, f := range files {
		_, isDirect := direct[f]
		syms := rankedSymbols(idx.fileDefs[f], idx.fileRefs[f], rank[f], opts.MaxSymbols)
		entries = append(entries, RepoMapEntry{Path: f, Score: rank[f], Direct: isDirect, Symbols: syms})
	}
	sort.SliceStable(entries, func(i, j int) bool {
		if entries[i].Direct != entries[j].Direct {
			return entries[i].Direct
		}
		if math.Abs(entries[i].Score-entries[j].Score) > 1e-12 {
			return entries[i].Score > entries[j].Score
		}
		return entries[i].Path < entries[j].Path
	})
	if len(entries) > opts.MaxFiles {
		entries = entries[:opts.MaxFiles]
	}
	for i := range entries {
		entries[i].Rank = i + 1
	}
	return entries
}
func rankedSymbols(defs, refs []string, fileRank float64, limit int) []RankedSymbol {
	seen := map[string]string{}
	for _, s := range refs {
		if strings.TrimSpace(s) != "" {
			seen[s] = "ref"
		}
	}
	for _, s := range defs {
		if strings.TrimSpace(s) != "" {
			seen[s] = "def"
		}
	}
	out := make([]RankedSymbol, 0, len(seen))
	for name, kind := range seen {
		score := fileRank
		if kind == "def" {
			score *= 2
		}
		out = append(out, RankedSymbol{Name: name, Score: score, Kind: kind})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Score != out[j].Score {
			return out[i].Score > out[j].Score
		}
		return out[i].Name < out[j].Name
	})
	if len(out) > limit {
		out = out[:limit]
	}
	for i := range out {
		out[i].Rank = i + 1
	}
	return out
}
func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
func maxInt2(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// FileDigest returns the indexed content digest for a relative path.
func (idx *Index) FileDigest(path string) string {
	if idx == nil {
		return ""
	}
	return idx.fileHashes[path]
}
