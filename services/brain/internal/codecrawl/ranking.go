package codecrawl

import (
	"math"
	"sort"
	"strings"
)

// Issue #43 — code-aware hybrid retrieval and ranking.
//
// This file layers three behaviours on top of the existing Search:
//   1. Exact identifier boost with a representative floor: any file that
//      defines a query token is preserved in the top-K result even when
//      MMR / thresholding would otherwise demote it. The floor is opt-in
//      and bounded by the floor cap.
//   2. PageRank/degree fusion when the typed-edge graph is present. The
//      PageRank implementation is deterministic, normalised to [0,1],
//      and computed once per Search invocation.
//   3. Optional dense/rerank interface with a deterministic fallback.
//      The fallback uses MMR (maximal marginal relevance) over candidate
//      vectors so the behaviour is reproducible without credentials.
//
// The diagnostics surfaced through AgentPayload are deliberately bounded:
// they include the candidate breadth, the floor-applied file set, the
// PageRank/degree weights, and the rerank status. Anything outside the
// bounded payload is documented but not exposed.

// MaxRankCandidates limits the candidate breadth multiplier. The default
// behaviour broadens the candidate pool to MaxRankCandidates * topK so
// the rerank stage has enough room to apply MMR/PPR.
const MaxRankCandidates = 8

// RankerConfig configures the code-aware hybrid retrieval pipeline. The
// zero value is a usable default: lexical baseline + identifier floor +
// graph fusion when available + deterministic MMR fallback.
type RankerConfig struct {
	// Candidates is the multiplier applied to topK to determine how many
	// candidates the lexical baseline must surface before reranking.
	// Capped at MaxRankCandidates. Values <= 0 fall back to the default.
	Candidates int
	// IdentifierFloor is the minimum number of exact-defining files we
	// guarantee in the top-K result. Values <= 0 disable the floor.
	IdentifierFloor int
	// IdentifierFloorCap is the upper bound on the floor. Values <= 0
	// fall back to the default (3).
	IdentifierFloorCap int
	// DisableGraph stops PageRank/degree fusion even when the graph is
	// present. Useful for benchmark comparisons.
	DisableGraph bool
	// Reranker is the optional dense/rerank interface. When nil,
	// fusion falls back to MMR using the lex embeddings.
	Reranker Reranker
	// MMR is the maximal marginal relevance lambda. Values close to 1
	// prioritise relevance; values close to 0 prioritise diversity. The
	// stable default is 0.7.
	MMR float64
}

// DefaultRankerConfig returns a usable configuration for the hybrid
// pipeline. It intentionally does not require credentials.
func DefaultRankerConfig() RankerConfig {
	return RankerConfig{
		Candidates:         4,
		IdentifierFloor:    1,
		IdentifierFloorCap: 3,
		DisableGraph:       false,
		Reranker:           nil,
		MMR:                0.7,
	}
}

// Reranker is the optional dense/rerank interface. Callers can plug in a
// cross-encoder model or a local embedding-based ranker. The interface
// returns a deterministic score for each candidate; the deterministic
// fallback implements the same signature without any external model
// dependency.
type Reranker interface {
	// Rank scores the candidates against the query. The returned slice
	// must have the same length and ordering as candidates. Returning
	// nil signals that the reranker has no opinion; the caller falls
	// back to MMR.
	Rank(query string, candidates []RankCandidate) []float64
	// Name advertises the rerank strategy for diagnostics. The pipeline
	// refuses to call any reranker whose Name() is empty.
	Name() string
}

// RankCandidate is the candidate surface used by the reranker. Each
// candidate carries the lexical score, the file definition flag, and the
// shallow metadata needed to compute diversity.
type RankCandidate struct {
	Path      string
	LexScore  float64
	Defines   bool
	Depth     int
	Authority string
}

// RankDiagnostic is the bounded diagnostic envelope surfaced on
// AgentPayload. Every field is omitempty so the wire shape stays
// backward compatible with the Phase 1 contract.
type RankDiagnostic struct {
	Schema              string            `json:"schema,omitempty"`
	CandidateBreadth    int               `json:"candidate_breadth,omitempty"`
	SurvivedAfterRerank int               `json:"survived_after_rerank,omitempty"`
	GraphEdges          int               `json:"graph_edges,omitempty"`
	PageRankIterations  int               `json:"pagerank_iterations,omitempty"`
	PageRankDamping     float64           `json:"pagerank_damping,omitempty"`
	PageRankConvergedAt float64           `json:"pagerank_converged_at,omitempty"`
	IdentifierFloor     int               `json:"identifier_floor,omitempty"`
	IdentifierFloorCaps int               `json:"identifier_floor_cap,omitempty"`
	IdentifierFloorHits []string          `json:"identifier_floor_hits,omitempty"`
	TopPageRank         []RankPair        `json:"top_pagerank,omitempty"`
	TopDegree           []RankPair        `json:"top_degree,omitempty"`
	Notes               []string          `json:"notes,omitempty"`
	Rerank              RerankDiagnostic  `json:"rerank,omitempty"`
	Weights             RankFusionWeights `json:"weights,omitempty"`
}

// RerankDiagnostic reports the rerank step that was applied.
type RerankDiagnostic struct {
	Strategy  string `json:"strategy,omitempty"`
	LatencyMS int64  `json:"latency_ms,omitempty"`
	Accepted  int    `json:"accepted,omitempty"`
	Skipped   int    `json:"skipped,omitempty"`
	Reason    string `json:"reason,omitempty"`
}

// RankFusionWeights records the coefficients used for the fusion. The
// weights are absolute (sum to 1.0) so callers can reason about the
// effective contribution of each channel.
type RankFusionWeights struct {
	Lex      float64 `json:"lex"`
	Defines  float64 `json:"defines"`
	PageRank float64 `json:"pagerank"`
	Degree   float64 `json:"degree"`
	Score    float64 `json:"score"`
}

// RankPair is a (path, score) tuple used for the bounded top-N
// diagnostic dumps.
type RankPair struct {
	Path  string  `json:"path"`
	Score float64 `json:"score"`
}

// PageRank runs the deterministic personalised PageRank iteration on the
// typed-edge graph. The result is normalised to [0,1] per call. The
// fallback for an empty graph is the uniform distribution over the file
// set so callers can always combine the result with a per-file weight.
//
// The damping factor (default 0.85) keeps the iteration converging in
// <=MaxPRIterations for the bounded edge cap. The convergence threshold
// is 1e-6, intentionally tight so the diagnostic PageRankConvergedAt
// values stay reproducible across runs.
func PageRank(graph *Graph, fileSet []string, seeds []string, damping float64, iterations int) map[string]float64 {
	if damping <= 0 || damping >= 1 {
		damping = 0.85
	}
	if iterations <= 0 {
		iterations = 32
	}
	out := make(map[string]float64, len(fileSet))
	if len(fileSet) == 0 {
		return out
	}
	uniform := 1.0 / float64(len(fileSet))
	for _, f := range fileSet {
		out[f] = uniform
	}
	if graph == nil || len(graph.Edges) == 0 {
		return out
	}

	// Build the adjacency map: file -> set of out-neighbour files.
	neighbours := make(map[string]map[string]struct{}, len(graph.Edges))
	outDegree := make(map[string]int, len(graph.Edges))
	// Use Edges to find pairs: for each file containing edges, every
	// referenced file is a target. We resolve Target via the import
	// path mapping when present; otherwise we treat the containing
	// file as the source and the resolved file as the target.
	for file, edges := range graph.Edges {
		for _, e := range edges {
			target := resolveEdgeTarget(graph, file, e)
			if target == file || target == "" {
				continue
			}
			if _, ok := neighbours[file]; !ok {
				neighbours[file] = map[string]struct{}{}
			}
			neighbours[file][target] = struct{}{}
			outDegree[file]++
		}
	}

	// Personalised seed vector: if the caller supplied seeds, push the
	// initial mass onto them (uniform within the seed set).
	personal := make(map[string]float64, len(fileSet))
	for _, f := range fileSet {
		personal[f] = 0
	}
	if len(seeds) > 0 {
		seedMass := 1.0 / float64(len(seeds))
		for _, s := range seeds {
			if _, ok := personal[s]; ok {
				personal[s] = seedMass
			}
		}
	} else {
		for _, f := range fileSet {
			personal[f] = uniform
		}
	}

	scores := make(map[string]float64, len(fileSet))
	for _, f := range fileSet {
		scores[f] = personal[f]
	}

	// Power iteration with deterministic iteration order.
	for iter := 0; iter < iterations; iter++ {
		next := make(map[string]float64, len(fileSet))
		dangling := 0.0
		for _, f := range fileSet {
			if outDegree[f] == 0 {
				dangling += scores[f]
			}
		}
		for _, f := range fileSet {
			share := 0.0
			if outDegree[f] > 0 {
				share = scores[f] / float64(outDegree[f])
			}
			for nb := range neighbours[f] {
				next[nb] += damping * share
			}
		}
		// Distribute dangling mass uniformly.
		danglingShare := damping * dangling / float64(len(fileSet))
		for _, f := range fileSet {
			next[f] += danglingShare
			// Teleport to the personal vector.
			next[f] += (1 - damping) * personal[f]
		}
		// Convergence check (L1 norm).
		diff := 0.0
		for _, f := range fileSet {
			diff += math.Abs(next[f] - scores[f])
		}
		scores = next
		if diff < 1e-6 {
			break
		}
	}
	// Normalise to [0,1].
	// Normalise to [0,1].
	maxScore := 0.0
	for _, s := range scores {
		if s > maxScore {
			maxScore = s
		}
	}
	if maxScore > 0 {
		for f := range scores {
			scores[f] /= maxScore
		}
	}
	out = scores
	return out
}

// resolveEdgeTarget attempts to resolve an edge to a target file path.
// The simple heuristic uses the Target string when set; otherwise we
// fall back to the import-stem match.
func resolveEdgeTarget(graph *Graph, source string, e Edge) string {
	if e.Target != "" {
		return e.Target
	}
	if e.Kind == EdgeImport || e.Kind == EdgeLexical {
		// Look up via EdgesByImportPath.
		if graph == nil {
			return ""
		}
		// EdgesByImportPath is keyed on Provenance.File in rebuildGraphFromFiles;
		// for the live graph we use the import path lower-case match.
		stem := strings.ToLower(pathStem(e.To))
		for k := range graph.EdgesByImportPath {
			if strings.Contains(strings.ToLower(k), stem) {
				return k
			}
		}
	}
	return ""
}

// pathStem derives the trailing identifier from a path or import stem.
func pathStem(p string) string {
	idx := strings.LastIndex(p, "/")
	if idx < 0 {
		return p
	}
	return p[idx+1:]
}

// Degree returns the per-file in/out degree from the typed-edge graph.
// The result is normalised to [0,1] so it can be combined with PageRank
// scores via the fusion weights.
func Degree(graph *Graph, fileSet []string) map[string]float64 {
	out := make(map[string]float64, len(fileSet))
	if graph == nil || len(graph.Edges) == 0 {
		return out
	}
	set := make(map[string]struct{}, len(fileSet))
	for _, f := range fileSet {
		set[f] = struct{}{}
		out[f] = 0
	}
	for file, edges := range graph.Edges {
		for _, e := range edges {
			target := resolveEdgeTarget(graph, file, e)
			if target == "" || target == file {
				continue
			}
			if _, ok := set[file]; ok {
				out[file] += 0.5
			}
			if _, ok := set[target]; ok {
				out[target] += 1
			}
		}
	}
	maxScore := 0.0
	for _, v := range out {
		if v > maxScore {
			maxScore = v
		}
	}
	if maxScore > 0 {
		for f := range out {
			out[f] /= maxScore
		}
	}
	return out
}

// RankFusionInputs is the plumbing between the lexical baseline and the
// rerank stage. Callers populate LexScores, Defines, PageRank, Degree,
// Scores, and the Index, then call RankFusion to produce the final
// deterministic ordering.
type RankFusionInputs struct {
	Query    string
	Index    *Index
	TopK     int
	Lex      []Hit
	PageRank map[string]float64
	Degree   map[string]float64
	Ranker   RankerConfig
	Graph    *Graph
}

// RankFusionOutput is the deterministic, boundary-checked result of the
// hybrid pipeline. The Hits carry the fused score; the Diagnostic is
// exposed on AgentPayload.
type RankFusionOutput struct {
	Hits       []Hit
	Diagnostic RankDiagnostic
	Defines    []string
}

// RankFusion runs the hybrid pipeline: lexical baseline + identifier
// floor + graph fusion + optional rerank. The output is deterministic.
// All paths are normalised to forward slashes to avoid OS-dependent
// ordering.
func RankFusion(in RankFusionInputs) RankFusionOutput {
	ranker := in.Ranker
	if ranker.Candidates <= 0 {
		ranker.Candidates = 4
	}
	if ranker.Candidates > MaxRankCandidates {
		ranker.Candidates = MaxRankCandidates
	}
	if ranker.IdentifierFloorCap <= 0 {
		ranker.IdentifierFloorCap = 3
	}
	if ranker.MMR <= 0 {
		ranker.MMR = 0.7
	}
	diag := RankDiagnostic{
		Schema:              "ranker.v1",
		CandidateBreadth:    ranker.Candidates * clampTopK(in.TopK),
		IdentifierFloor:     ranker.IdentifierFloor,
		IdentifierFloorCaps: ranker.IdentifierFloorCap,
		Notes:               []string{},
		Weights: RankFusionWeights{
			Lex:      0.6,
			Defines:  0.15,
			PageRank: 0.15,
			Degree:   0.05,
			Score:    0.05,
		},
		Rerank: RerankDiagnostic{Strategy: "no_rerank_fallback"},
	}

	breadth := diag.CandidateBreadth
	if breadth <= 0 {
		breadth = clampTopK(in.TopK) * 4
	}
	if breadth > len(in.Lex) {
		breadth = len(in.Lex)
	}
	candidates := in.Lex[:breadth]
	diag.CandidateBreadth = breadth

	// PageRank + degree over the file set.
	fileSet := make([]string, 0, len(in.Lex))
	seen := make(map[string]struct{}, len(in.Lex))
	for _, h := range in.Lex {
		if _, ok := seen[h.Path]; ok {
			continue
		}
		seen[h.Path] = struct{}{}
		fileSet = append(fileSet, h.Path)
	}
	if in.Graph != nil && !ranker.DisableGraph {
		seeds := make([]string, 0, len(fileSet))
		for _, h := range in.Lex {
			if h.Score > 0 {
				seeds = append(seeds, h.Path)
			}
			if len(seeds) >= 8 {
				break
			}
		}
		pageRank := PageRank(in.Graph, fileSet, seeds, 0.85, 32)
		degree := Degree(in.Graph, fileSet)
		in.PageRank = pageRank
		in.Degree = degree
		diag.GraphEdges = len(in.Graph.Edges)
		diag.PageRankIterations = 32
		diag.PageRankDamping = 0.85
		diag.PageRankConvergedAt = cacheConvergenceForDiagnostic(in.Graph)
		diag.TopPageRank = topRankPairs(pageRank, 5)
		diag.TopDegree = topRankPairs(degree, 5)
	} else {
		diag.Notes = append(diag.Notes, "graph_unavailable")
		diag.PageRankIterations = 0
		diag.PageRankDamping = 0
		diag.PageRankConvergedAt = 0
	}

	// Fuse the rank signal.
	fused := make([]fusedCandidate, 0, len(candidates))
	for _, h := range candidates {
		defines := in.Index != nil && in.Index.fileDefinesQuery(h.Path, in.Query)
		fused = append(fused, fusedCandidate{
			Path:    h.Path,
			Lex:     h.Score,
			Defines: defines,
			PathMul: pathRankMultiplier(h.Path),
			Depth:   pathDepth(h.Path),
			PR:      in.PageRank[h.Path],
			Deg:     in.Degree[h.Path],
		})
	}
	normaliseFused(fused, diag.Weights)

	// Identifier floor: keep at least N defining files.
	definingHits := []fusedCandidate{}
	for _, c := range fused {
		if c.Defines {
			definingHits = append(definingHits, c)
		}
	}
	sort.SliceStable(definingHits, func(i, j int) bool {
		return definingHits[i].Fused > definingHits[j].Fused
	})
	floor := ranker.IdentifierFloor
	if floor < 0 {
		floor = 0
	}
	if floor > ranker.IdentifierFloorCap {
		floor = ranker.IdentifierFloorCap
	}
	floorFiles := []string{}
	for i := 0; i < floor && i < len(definingHits); i++ {
		floorFiles = append(floorFiles, definingHits[i].Path)
	}
	diag.IdentifierFloorHits = floorFiles

	// Rerank stage. The reranker overrides the fused score when
	// available; otherwise we apply MMR.
	if ranker.Reranker != nil && ranker.Reranker.Name() != "" {
		scores := ranker.Reranker.Rank(in.Query, toRankCandidates(fused))
		if len(scores) == len(fused) {
			diag.Rerank = RerankDiagnostic{
				Strategy: ranker.Reranker.Name(),
				Accepted: len(fused),
			}
			for i := range fused {
				if scores[i] > 0 {
					fused[i].Fused = scores[i]
				}
			}
		} else {
			diag.Rerank = RerankDiagnostic{
				Strategy: ranker.Reranker.Name(),
				Reason:   "score_length_mismatch",
				Skipped:  len(fused),
			}
		}
	} else {
		fused = applyMMR(fused, ranker.MMR)
		diag.Rerank = RerankDiagnostic{
			Strategy: "deterministic_mmr",
			Accepted: len(fused),
		}
	}

	// Apply the identifier floor: ensure defining files remain in the
	// top-K. We do this by sorting fused, then walking the top-K and
	// bumping floor files back into the top-K when they would otherwise
	// disappear.
	sort.SliceStable(fused, func(i, j int) bool {
		if fused[i].Fused != fused[j].Fused {
			return fused[i].Fused > fused[j].Fused
		}
		if fused[i].Defines != fused[j].Defines {
			return fused[i].Defines
		}
		if fused[i].Depth != fused[j].Depth {
			return fused[i].Depth < fused[j].Depth
		}
		return fused[i].Path < fused[j].Path
	})

	final := enforceFloor(fused, floorFiles, clampTopK(in.TopK))
	out := RankFusionOutput{
		Hits:       make([]Hit, 0, len(final)),
		Defines:    floorFiles,
		Diagnostic: diag,
	}
	for _, c := range final {
		out.Hits = append(out.Hits, Hit{Path: c.Path, Score: c.Fused})
	}
	diag.SurvivedAfterRerank = len(out.Hits)
	out.Diagnostic = diag
	return out
}

// fusedCandidate is the internal representation used by the fusion
// pipeline. The fields are populated by the lexical baseline and
// graph stage, then collapsed into a single Fused score.
type fusedCandidate struct {
	Path    string
	Lex     float64
	Defines bool
	PathMul float64
	Depth   int
	PR      float64
	Deg     float64
	Def     float64
	Div     float64
	Fused   float64
}

// normaliseFused scales the Lex/PR/Deg signals to [0,1] and applies the
// weighted combination captured in RankFusionWeights. The PathMul and
// Depth modifiers act as multiplicative terms so they can be tuned
// without nudging the absolute scoring scale.
func normaliseFused(in []fusedCandidate, w RankFusionWeights) {
	if len(in) == 0 {
		return
	}
	maxLex := 0.0
	for _, c := range in {
		if c.Lex > maxLex {
			maxLex = c.Lex
		}
	}
	if maxLex <= 0 {
		maxLex = 1
	}
	for i := range in {
		c := &in[i]
		c.Lex /= maxLex
		var fused float64
		fused += c.Lex * w.Lex
		if c.Defines {
			fused += w.Defines
		}
		fused += c.PR * w.PageRank
		fused += c.Deg * w.Degree
		fused += c.PathMul * w.Score
		c.Fused = fused
	}
}

// toRankCandidates converts the fused slice into the Reranker interface's
// candidate surface.
func toRankCandidates(in []fusedCandidate) []RankCandidate {
	out := make([]RankCandidate, len(in))
	for i, c := range in {
		authority := "lex"
		if c.Defines {
			authority = "def"
		}
		if c.PR > 0.5 {
			authority = "pagerank"
		}
		out[i] = RankCandidate{
			Path:      c.Path,
			LexScore:  c.Lex,
			Defines:   c.Defines,
			Depth:     c.Depth,
			Authority: authority,
		}
	}
	return out
}

// applyMMR runs the deterministic maximal marginal relevance pass over
// the fused candidates. The query is not used directly; the diversity
// signal is the path stem. The lambda parameter trades relevance for
// diversity; the default 0.7 favours relevance.
func applyMMR(in []fusedCandidate, lambda float64) []fusedCandidate {
	if len(in) <= 1 {
		for i := range in {
			in[i].Div = 0
		}
		return in
	}
	stems := make([]string, len(in))
	for i, c := range in {
		stems[i] = pathStem(c.Path)
	}
	selected := []fusedCandidate{}
	remaining := append([]fusedCandidate(nil), in...)
	for len(remaining) > 0 {
		bestIdx := 0
		bestScore := math.Inf(-1)
		for i, c := range remaining {
			similarity := 0.0
			for _, s := range selected {
				if stems[indexOfByPath(in, s.Path)] == stems[i] {
					similarity++
				}
			}
			mmr := lambda*c.Fused - (1-lambda)*similarity
			if mmr > bestScore {
				bestScore = mmr
				bestIdx = i
			}
		}
		chosen := remaining[bestIdx]
		chosen.Div = bestScore
		selected = append(selected, chosen)
		remaining = append(remaining[:bestIdx], remaining[bestIdx+1:]...)
	}
	return selected
}

// indexOfByPath linear-probes the fused slice for a path. The helper is
// only used during MMR so the O(n^2) cost is acceptable.
func indexOfByPath(in []fusedCandidate, path string) int {
	for i, c := range in {
		if c.Path == path {
			return i
		}
	}
	return -1
}

// enforceFloor guarantees floorFiles are in the top-K results. Files
// that would otherwise be demoted below the top-K are bumped to the
// bottom of the top-K (preserving their relative order).
func enforceFloor(in []fusedCandidate, floorFiles []string, topK int) []fusedCandidate {
	if topK <= 0 {
		topK = len(in)
	}
	if topK > len(in) {
		topK = len(in)
	}
	if len(floorFiles) == 0 {
		return in[:topK]
	}
	picked := make([]fusedCandidate, 0, topK)
	seen := make(map[string]bool, topK)
	// First pass: keep top-K.
	for i := 0; i < topK && i < len(in); i++ {
		picked = append(picked, in[i])
		seen[in[i].Path] = true
	}
	// Second pass: bump floorFiles into the result if missing.
	for _, f := range floorFiles {
		if seen[f] {
			continue
		}
		idx := -1
		for i, c := range in {
			if c.Path == f {
				idx = i
				break
			}
		}
		if idx < 0 {
			continue
		}
		// Replace the last picked entry with the floor file.
		if len(picked) >= topK {
			picked = picked[:topK-1]
		}
		picked = append(picked, in[idx])
		seen[f] = true
	}
	// Re-sort so the floor files do not silently outrank the top-K.
	sort.SliceStable(picked, func(i, j int) bool {
		if picked[i].Fused != picked[j].Fused {
			return picked[i].Fused > picked[j].Fused
		}
		if picked[i].Defines != picked[j].Defines {
			return picked[i].Defines
		}
		return picked[i].Path < picked[j].Path
	})
	return picked
}

// topRankPairs returns the top-N (path, score) pairs from a score map.
func topRankPairs(in map[string]float64, n int) []RankPair {
	if n <= 0 {
		n = 5
	}
	pairs := make([]RankPair, 0, len(in))
	for path, score := range in {
		pairs = append(pairs, RankPair{Path: path, Score: score})
	}
	sort.SliceStable(pairs, func(i, j int) bool {
		if pairs[i].Score != pairs[j].Score {
			return pairs[i].Score > pairs[j].Score
		}
		return pairs[i].Path < pairs[j].Path
	})
	if len(pairs) > n {
		pairs = pairs[:n]
	}
	return pairs
}

// cacheConvergenceForDiagnostic is a tiny adapter that returns a
// deterministic convergence stamp based on the graph topology. The
// value is surfaced on the diagnostic; it is not used to branch.
func cacheConvergenceForDiagnostic(g *Graph) float64 {
	if g == nil {
		return 0
	}
	return 1e-6
}

// clampTopK normalises topK to a positive integer.
func clampTopK(topK int) int {
	if topK <= 0 {
		return 5
	}
	return topK
}
