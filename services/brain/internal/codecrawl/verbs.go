package codecrawl

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/repoignore"
)

// AgentHit is a lean agent-facing hit (SCM find_relevant style).
type AgentHit struct {
	Path      string  `json:"path"`
	Score     float64 `json:"score"`
	Kind      string  `json:"kind,omitempty"` // def|ref|import|lexical
	Preview   string  `json:"preview,omitempty"`
	StartLine int     `json:"start_line,omitempty"`
}

// AgentPayload is a compact retrieval envelope (SCM AgentPayload analogue).
type AgentPayload struct {
	Query     string     `json:"query,omitempty"`
	Hits      []AgentHit `json:"hits"`
	KeepMin   float64    `json:"keep_min"`
	Truncated bool       `json:"truncated"`
	Notes     []string   `json:"notes,omitempty"`
}

// RankedAgentPayload extends AgentPayload with the bounded hybrid
// retrieval diagnostics. The shape is additive on top of AgentPayload so
// existing consumers keep their parsing invariants.
type RankedAgentPayload struct {
	AgentPayload
	Diagnostic RankDiagnostic `json:"diagnostic,omitempty"`
	Defines    []string       `json:"defines,omitempty"`
}

// ImpactReceipt is a heuristic impact closure over symbol defs/refs/imports.
//
// Existing JSON fields stay byte-compatible with the Phase 1 contract:
// callers decode by field name and tolerate new optional fields. New
// fields (Truncated, Severity, AffectedTests, ChangedSymbols, Schema) are
// additive and omitempty so legacy fixtures still decode cleanly.
//
// Severity is one of {info, low, medium, high}; AffectedTests is the
// deterministic subset of Closure that looks like test files; ChangedSymbols
// is the bounded list of symbols touched when the seed is a file.
type ImpactReceipt struct {
	Seed         string   `json:"seed"`
	SeedKind     string   `json:"seed_kind"` // symbol|file
	Direct       []string `json:"direct"`
	Closure      []string `json:"closure"`
	Unknowns     []string `json:"unknowns,omitempty"`
	CoverageGaps []string `json:"coverage_gaps"`
	Authority    string   `json:"authority"` // heuristic
	MaxDepth     int      `json:"max_depth"`
	DurationMS   int64    `json:"duration_ms"`
	SymbolDefs   int      `json:"symbol_defs_matched"`
	SymbolRefs   int      `json:"symbol_refs_matched"`
	// Phase 2: explicit truncation signal. True when the closure hit
	// maxFiles or any per-arm cap before BFS exhausted.
	Truncated bool `json:"truncated,omitempty"`
	// Severity classifies the blast-radius size (low | medium | high).
	Severity string `json:"severity,omitempty"`
	// AffectedTests is the deterministic test-file subset of Closure.
	AffectedTests []string `json:"affected_tests,omitempty"`
	// ChangedSymbols reports the bounded symbols touched when seed is file.
	ChangedSymbols []string `json:"changed_symbols,omitempty"`
	// Schema is the receipt schema version. Bumped to "v2" for Phase 2
	// so downstream tooling can branch safely.
	Schema string `json:"schema,omitempty"`
}

// FreshnessReport is a cheap workspace vs index probe.
type FreshnessReport struct {
	Root           string `json:"root"`
	GobPath        string `json:"gob_path,omitempty"`
	Fresh          bool   `json:"fresh"`
	GitHeadIndex   string `json:"git_head_index,omitempty"`
	GitHeadWork    string `json:"git_head_worktree,omitempty"`
	IndexedFiles   int    `json:"indexed_files"`
	WalkedFiles    int    `json:"walked_files"`
	StampMatch     int    `json:"stamp_match"`
	StampMismatch  int    `json:"stamp_mismatch"`
	MissingInIndex int    `json:"missing_in_index"`
	DeletedOnDisk  int    `json:"deleted_on_disk"`
	DurationMS     int64  `json:"duration_ms"`
	Schema         string `json:"schema,omitempty"`
}

// FindRelevant returns a lean top-k payload with optional source previews.
func (idx *Index) FindRelevant(root, query string, topK int, withPreview bool) AgentPayload {
	if topK <= 0 {
		topK = 5
	}
	hits := idx.SearchOpts(query, topK*2, true)
	keepMin := 0.15
	out := AgentPayload{Query: query, KeepMin: keepMin, Notes: []string{"codecrawl_tf_symbol_hop"}}
	maxScore := 0.0
	for _, h := range hits {
		if h.Score > maxScore {
			maxScore = h.Score
		}
	}
	for _, h := range hits {
		// Soft floor relative to best hit.
		if maxScore > 0 && h.Score/maxScore < keepMin && len(out.Hits) > 0 {
			out.Truncated = true
			continue
		}
		ah := AgentHit{Path: h.Path, Score: h.Score, Kind: "lexical"}
		if idx.fileDefinesQuery(h.Path, query) {
			ah.Kind = "def"
		}
		if withPreview && root != "" {
			if abs, ok := safeRootPath(root, h.Path); ok {
				ah.Preview, ah.StartLine = previewFile(abs, 12)
			}
		}
		out.Hits = append(out.Hits, ah)
		if len(out.Hits) >= topK {
			break
		}
	}
	return out
}

// FindRelevantRanked runs the hybrid retrieval pipeline (lexical baseline
// + identifier floor + PageRank/degree fusion + deterministic MMR
// rerank). The method is credentials-free and exposes the bounded
// RankDiagnostic on the result. The returned hits are sorted by fused
// score, with the identifier floor guaranteed in the top-K.
func (idx *Index) FindRelevantRanked(root, query string, topK int, withPreview bool, config RankerConfig) RankedAgentPayload {
	if topK <= 0 {
		topK = 5
	}
	if config.Candidates <= 0 {
		config.Candidates = 4
	}
	candidates := idx.SearchOpts(query, topK*config.Candidates, true)
	fusion := RankFusion(RankFusionInputs{
		Query:  query,
		Index:  idx,
		TopK:   topK,
		Lex:    candidates,
		Ranker: config,
		Graph:  idx.Graph(),
	})
	out := RankedAgentPayload{
		AgentPayload: AgentPayload{
			Query:   query,
			Hits:    make([]AgentHit, 0, len(fusion.Hits)),
			KeepMin: 0.0,
			Notes:   []string{"codecrawl_ranked_v1"},
		},
		Diagnostic: fusion.Diagnostic,
		Defines:    fusion.Defines,
	}
	for _, h := range fusion.Hits {
		ah := AgentHit{Path: h.Path, Score: h.Score, Kind: "lexical"}
		if idx.fileDefinesQuery(h.Path, query) {
			ah.Kind = "def"
		}
		if withPreview && root != "" {
			if abs, ok := safeRootPath(root, h.Path); ok {
				ah.Preview, ah.StartLine = previewFile(abs, 12)
			}
		}
		out.Hits = append(out.Hits, ah)
	}
	return out
}

func (idx *Index) fileDefinesQuery(path, query string) bool {
	if idx == nil {
		return false
	}
	q := tokenize(query)
	defs := idx.fileDefs[path]
	for _, d := range defs {
		dl := strings.ToLower(d)
		for _, t := range q {
			if dl == t {
				return true
			}
		}
	}
	return false
}

// RouteReceipt is a path-bridge between two seeds (SCM find_route analogue).
// Heuristic: shared symbol names and import edges — not precise stack-graph paths.
type RouteReceipt struct {
	From       string   `json:"from"`
	To         string   `json:"to"`
	Bridges    []string `json:"bridges"`
	ViaSymbols []string `json:"via_symbols,omitempty"`
	Authority  string   `json:"authority"`
	Notes      []string `json:"notes,omitempty"`
}

// DefsOf returns files that define a symbol name (SCM defs verb).
func (idx *Index) DefsOf(name string) []string {
	if idx == nil || strings.TrimSpace(name) == "" {
		return nil
	}
	out := uniqueStrings(idx.resolveSeedFiles(name))
	sort.Strings(out)
	return out
}

// RefsOf returns files that reference a symbol name (SCM refs verb).
func (idx *Index) RefsOf(name string) []string {
	if idx == nil || strings.TrimSpace(name) == "" {
		return nil
	}
	name = strings.TrimSpace(name)
	var out []string
	low := strings.ToLower(name)
	if idx.symbols != nil {
		out = append(out, idx.symbols.Refs[name]...)
		out = append(out, idx.symbols.Refs[low]...)
	}
	refPaths := make([]string, 0, len(idx.fileRefs))
	for path := range idx.fileRefs {
		refPaths = append(refPaths, path)
	}
	sort.Strings(refPaths)
	for _, path := range refPaths {
		for _, r := range idx.fileRefs[path] {
			if strings.EqualFold(r, name) {
				out = append(out, path)
			}
		}
	}
	out = uniqueStrings(out)
	sort.Strings(out)
	return out
}

// FindRoute finds heuristic bridges between two file paths or symbol names.
// Bridges are intermediate files that share def/ref/import names with both ends.
func (idx *Index) FindRoute(from, to string, maxBridges int) RouteReceipt {
	rec := RouteReceipt{From: from, To: to, Authority: "heuristic", Notes: []string{"no_stack_graph_dsl", "name_cooccurrence_bridges"}}
	if idx == nil || strings.TrimSpace(from) == "" || strings.TrimSpace(to) == "" {
		rec.Notes = append(rec.Notes, "empty_seed")
		return rec
	}
	if maxBridges <= 0 {
		maxBridges = 12
	}
	// Resolve seeds to files.
	fromFiles := idx.resolveSeedFiles(from)
	toFiles := idx.resolveSeedFiles(to)
	if len(fromFiles) == 0 || len(toFiles) == 0 {
		rec.Notes = append(rec.Notes, "unresolved_seed")
		return rec
	}
	// Symbol names defined or referenced by from-side.
	fromNames := map[string]struct{}{}
	toNames := map[string]struct{}{}
	for _, f := range fromFiles {
		for _, d := range idx.fileDefs[f] {
			fromNames[strings.ToLower(d)] = struct{}{}
		}
		for _, r := range idx.fileRefs[f] {
			fromNames[strings.ToLower(r)] = struct{}{}
		}
	}
	for _, f := range toFiles {
		for _, d := range idx.fileDefs[f] {
			toNames[strings.ToLower(d)] = struct{}{}
		}
		for _, r := range idx.fileRefs[f] {
			toNames[strings.ToLower(r)] = struct{}{}
		}
	}
	// Shared names.
	var via []string
	for n := range fromNames {
		if _, ok := toNames[n]; ok {
			via = append(via, n)
		}
	}
	sort.Strings(via)
	if len(via) > 8 {
		via = via[:8]
	}
	rec.ViaSymbols = via
	// Bridge files: files that define/ref shared names, excluding endpoints.
	end := map[string]struct{}{}
	for _, f := range fromFiles {
		end[f] = struct{}{}
	}
	for _, f := range toFiles {
		end[f] = struct{}{}
	}
	bridgeScore := map[string]int{}
	for _, n := range via {
		if idx.symbols != nil {
			for _, f := range idx.symbols.Defs[n] {
				if _, skip := end[f]; !skip {
					bridgeScore[f]++
				}
			}
			for _, f := range idx.symbols.Refs[n] {
				if _, skip := end[f]; !skip {
					bridgeScore[f]++
				}
			}
		}
	}
	type sc struct {
		f string
		n int
	}
	var ranked []sc
	for f, n := range bridgeScore {
		ranked = append(ranked, sc{f, n})
	}
	sort.Slice(ranked, func(i, j int) bool {
		if ranked[i].n != ranked[j].n {
			return ranked[i].n > ranked[j].n
		}
		return ranked[i].f < ranked[j].f
	})
	for i, r := range ranked {
		if i >= maxBridges {
			break
		}
		rec.Bridges = append(rec.Bridges, r.f)
	}
	// If no bridges, hop expand from from-side toward to.
	if len(rec.Bridges) == 0 {
		hop := idx.SymbolHop(fromFiles, maxBridges)
		for _, h := range hop {
			if _, skip := end[h]; skip {
				continue
			}
			rec.Bridges = append(rec.Bridges, h)
			if len(rec.Bridges) >= maxBridges {
				break
			}
		}
		rec.Notes = append(rec.Notes, "fallback_symbol_hop")
	}
	return rec
}

func (idx *Index) resolveSeedFiles(seed string) []string {
	seed = strings.TrimSpace(seed)
	if seed == "" {
		return nil
	}
	if strings.Contains(seed, "/") || strings.Contains(seed, "\\") {
		if _, ok := idx.files[seed]; ok {
			return []string{seed}
		}
		// Prefix match. Sort before applying the cap so path aliases resolve
		// reproducibly across map iterations.
		matches := make([]string, 0)
		for p := range idx.files {
			if strings.HasSuffix(p, seed) || strings.Contains(p, seed) {
				matches = append(matches, p)
			}
		}
		sort.Strings(matches)
		if len(matches) > 5 {
			matches = matches[:5]
		}
		return matches
	}
	// Symbol name.
	var out []string
	if idx.symbols != nil {
		out = append(out, idx.symbols.Defs[seed]...)
		out = append(out, idx.symbols.Defs[strings.ToLower(seed)]...)
	}
	for path, defs := range idx.fileDefs {
		for _, d := range defs {
			if strings.EqualFold(d, seed) {
				out = append(out, path)
			}
		}
	}
	out = uniqueStrings(out)
	sort.Strings(out)
	return out
}

// Expand grows a seed path/symbol set via symbol hop (SCM expand analogue).
func (idx *Index) Expand(seeds []string, maxN int) []Hit {
	if maxN <= 0 {
		maxN = 12
	}
	// If seed looks like a symbol name (no path sep), resolve defining files first.
	var seedFiles []string
	for _, s := range seeds {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		if strings.Contains(s, "/") || strings.Contains(s, "\\") {
			seedFiles = append(seedFiles, s)
			continue
		}
		// Symbol name.
		if idx.symbols != nil {
			for _, f := range idx.symbols.Defs[s] {
				seedFiles = append(seedFiles, f)
			}
			for _, f := range idx.symbols.Defs[strings.ToLower(s)] {
				seedFiles = append(seedFiles, f)
			}
		}
		for path, defs := range idx.fileDefs {
			for _, d := range defs {
				if strings.EqualFold(d, s) {
					seedFiles = append(seedFiles, path)
				}
			}
		}
	}
	seedFiles = uniqueStrings(seedFiles)
	sort.Strings(seedFiles)
	neighbors := idx.SymbolHop(seedFiles, maxN)
	out := make([]Hit, 0, len(seedFiles)+len(neighbors))
	for _, f := range seedFiles {
		out = append(out, Hit{Path: f, Score: 2})
	}
	have := map[string]struct{}{}
	for _, f := range seedFiles {
		have[f] = struct{}{}
	}
	for _, n := range neighbors {
		if _, ok := have[n]; ok {
			continue
		}
		out = append(out, Hit{Path: n, Score: 1 * pathRankMultiplier(n)})
		have[n] = struct{}{}
		if len(out) >= maxN {
			break
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Score != out[j].Score {
			return out[i].Score > out[j].Score
		}
		return out[i].Path < out[j].Path
	})
	return out
}

// Impact builds a heuristic impact radius for a symbol name or file path.
// Authority is always "heuristic" (not LSP/SCIP). Coverage gaps are explicit.
//
// Phase 2: when the index carries a typed-edge projection (Index.Graph !=
// nil), Impact surfaces call-aware selection by joining callersFor() into
// Direct before BFS, and reports a deterministic Severity / AffectedTests
// / Truncated triple. When the graph is absent (legacy gob snapshot),
// Impact degrades to the Phase 1 name-graph heuristic without changing
// any pre-existing JSON field.
func (idx *Index) Impact(seed string, maxDepth, maxFiles int) ImpactReceipt {
	t0 := time.Now()
	if maxDepth <= 0 {
		maxDepth = 2
	}
	if maxFiles <= 0 {
		maxFiles = 40
	}
	rec := ImpactReceipt{
		Seed:         seed,
		Authority:    "heuristic", // name graph + import edges; not LSP/SCIP (SCM-006)
		MaxDepth:     maxDepth,
		CoverageGaps: []string{"no_full_call_graph", "no_lsp_server", "no_type_flow"},
		Schema:       "v2",
	}
	seed = strings.TrimSpace(seed)
	if seed == "" {
		rec.Unknowns = []string{"empty_seed"}
		rec.CoverageGaps = append(rec.CoverageGaps, "unknown_seed")
		rec.DurationMS = time.Since(t0).Milliseconds()
		return rec
	}

	// Collect seed files + symbol names.
	var seedFiles []string
	var names []string
	if strings.Contains(seed, "/") || strings.Contains(seed, "\\") || strings.HasSuffix(seed, ".go") || strings.HasSuffix(seed, ".ts") {
		rec.SeedKind = "file"
		seedFiles = []string{seed}
		if defs := idx.fileDefs[seed]; len(defs) > 0 {
			names = append(names, defs...)
		}
		if refs := idx.fileRefs[seed]; len(refs) > 0 {
			// Prefer exported-looking names as hop seeds.
			for _, r := range refs {
				if len(r) >= 3 {
					names = append(names, r)
				}
			}
		}
	} else {
		rec.SeedKind = "symbol"
		names = []string{seed, strings.ToLower(seed)}
		if idx.symbols != nil {
			seedFiles = append(seedFiles, idx.symbols.Defs[seed]...)
			seedFiles = append(seedFiles, idx.symbols.Defs[strings.ToLower(seed)]...)
		}
		for path, defs := range idx.fileDefs {
			for _, d := range defs {
				if strings.EqualFold(d, seed) {
					seedFiles = append(seedFiles, path)
				}
			}
		}
	}
	seedFiles = uniqueStrings(seedFiles)
	sort.Strings(seedFiles)
	names = uniqueStrings(names)
	sort.Strings(names)
	rec.SymbolDefs = len(seedFiles)

	// Direct: defining files first; refs ranked and capped (popular symbols
	// like TextModel must not dump thousands of call sites as "direct").
	directSet := map[string]struct{}{}
	for _, f := range seedFiles {
		directSet[f] = struct{}{}
	}
	var refHits []Hit
	if idx.symbols != nil {
		for _, n := range names {
			for _, f := range idx.symbols.Defs[n] {
				directSet[f] = struct{}{}
			}
			for _, f := range idx.symbols.Defs[strings.ToLower(n)] {
				directSet[f] = struct{}{}
			}
			for _, f := range idx.symbols.Refs[n] {
				refHits = append(refHits, Hit{Path: f, Score: pathRankMultiplier(f)})
				rec.SymbolRefs++
			}
			for _, f := range idx.symbols.Refs[strings.ToLower(n)] {
				refHits = append(refHits, Hit{Path: f, Score: pathRankMultiplier(f)})
			}
		}
	}
	for path, defs := range idx.fileDefs {
		for _, d := range defs {
			for _, n := range names {
				if strings.EqualFold(d, n) {
					directSet[path] = struct{}{}
				}
			}
		}
	}
	// Cap refs: keep top production-weighted referrers only.
	sort.SliceStable(refHits, func(i, j int) bool {
		if refHits[i].Score != refHits[j].Score {
			return refHits[i].Score > refHits[j].Score
		}
		return refHits[i].Path < refHits[j].Path
	})
	refCap := 24
	seenRef := map[string]struct{}{}
	for _, h := range refHits {
		if _, ok := seenRef[h.Path]; ok {
			continue
		}
		if _, ok := directSet[h.Path]; ok {
			continue
		}
		seenRef[h.Path] = struct{}{}
		directSet[h.Path] = struct{}{}
		if len(seenRef) >= refCap {
			break
		}
	}

	// Import-neighbor expansion for seed file stems (capped).
	impCap := 16
	impN := 0
	impPaths := make([]string, 0, len(idx.fileImps))
	for path := range idx.fileImps {
		impPaths = append(impPaths, path)
	}
	sort.Strings(impPaths)
	for _, sf := range seedFiles {
		base := filepath.Base(sf)
		stem := strings.TrimSuffix(base, filepath.Ext(base))
		for _, path := range impPaths {
			imps := idx.fileImps[path]
			if impN >= impCap {
				break
			}
			for _, imp := range imps {
				if strings.Contains(strings.ToLower(imp), strings.ToLower(stem)) {
					if _, ok := directSet[path]; !ok {
						directSet[path] = struct{}{}
						impN++
					}
					break
				}
			}
		}
	}

	// Phase 2 call-aware selection: when the typed-edge projection is
	// available, augment Direct with files that demonstrably call the seed
	// symbol. This is bounded and recorded so the receipt can show whether
	// call-aware selection fired. Symbols with no callers fall back to
	// the Phase 1 path silently.
	if rec.SeedKind == "symbol" && len(names) > 0 && idx.HasGraph() {
		graphAdded := 0
		graphCap := 24
		for _, n := range names {
			added, hit := callAwareDirect(idx, n, directSet, graphCap-graphAdded)
			graphAdded += len(added)
			if hit {
				rec.Truncated = true
			}
			if graphAdded >= graphCap {
				rec.Truncated = true
				break
			}
		}
	}

	// Phase 2 file-seed case: surface the bounded list of symbols touched
	// inside the seed file so callers can pick a more specific seed.
	if rec.SeedKind == "file" {
		if defs, ok := idx.fileDefs[seed]; ok {
			cp := append([]string(nil), defs...)
			sort.Strings(cp)
			if len(cp) > 24 {
				cp = cp[:24]
				rec.Truncated = true
			}
			rec.ChangedSymbols = cp
		}
	}

	for f := range directSet {
		rec.Direct = append(rec.Direct, f)
	}
	sort.SliceStable(rec.Direct, func(i, j int) bool {
		mi, mj := pathRankMultiplier(rec.Direct[i]), pathRankMultiplier(rec.Direct[j])
		if mi != mj {
			return mi > mj
		}
		return rec.Direct[i] < rec.Direct[j]
	})

	// Closure: BFS over symbol hop from direct set, depth-limited.
	frontier := append([]string(nil), rec.Direct...)
	closure := map[string]struct{}{}
	for _, f := range rec.Direct {
		closure[f] = struct{}{}
	}
	closureTruncated := false
	for depth := 0; depth < maxDepth; depth++ {
		nextHop := idx.SymbolHop(frontier, maxFiles)
		var next []string
		for _, n := range nextHop {
			if _, ok := closure[n]; ok {
				continue
			}
			closure[n] = struct{}{}
			next = append(next, n)
			if len(closure) >= maxFiles {
				closureTruncated = true
				break
			}
		}
		frontier = next
		if len(frontier) == 0 || len(closure) >= maxFiles {
			break
		}
	}
	for f := range closure {
		rec.Closure = append(rec.Closure, f)
	}
	sort.Strings(rec.Closure)
	// Rank closure with production preference for stable top.
	sort.SliceStable(rec.Closure, func(i, j int) bool {
		mi, mj := pathRankMultiplier(rec.Closure[i]), pathRankMultiplier(rec.Closure[j])
		if mi != mj {
			return mi > mj
		}
		return rec.Closure[i] < rec.Closure[j]
	})
	if closureTruncated {
		rec.Truncated = true
	}
	if len(rec.Direct) == 0 {
		rec.Unknowns = append(rec.Unknowns, "no_defs_or_refs_found")
	}
	if !idx.HasGraph() {
		// Fail-closed: when the typed-edge projection is absent (legacy
		// gob snapshot), explicitly tag the receipt so callers can branch
		// on authority without inferring absence from empty fields.
		rec.CoverageGaps = append(rec.CoverageGaps, "graph_unavailable")
	}
	rec.Severity = severityForClosure(len(rec.Direct), len(rec.Closure))
	rec.AffectedTests = detectAffectedTests(rec.Closure)
	if len(rec.Closure) == 0 && len(rec.Direct) == 0 {
		rec.CoverageGaps = append(rec.CoverageGaps, "no_resolution")
	}
	rec.DurationMS = time.Since(t0).Milliseconds()
	return rec
}

// Freshness probes the workspace vs a loaded index using stamps only (no content read).
func (idx *Index) Freshness(root string, meta DurableMeta) FreshnessReport {
	t0 := time.Now()
	rootAbs, _ := filepath.Abs(root)
	rep := FreshnessReport{
		Root:         rootAbs,
		GitHeadIndex: meta.GitHead,
		GitHeadWork:  gitHead(rootAbs),
		IndexedFiles: idx.FileCount(),
		Schema:       meta.Schema,
	}
	ignores, _ := repoignore.Load(rootAbs)
	var walked []string
	_ = filepath.Walk(rootAbs, func(path string, info os.FileInfo, err error) error {
		if err != nil || info == nil {
			return nil
		}
		rel, relErr := filepath.Rel(rootAbs, path)
		if relErr != nil {
			return nil
		}
		if ignores != nil && ignores.Ignored(rel, info.IsDir()) {
			if info.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if info.IsDir() {
			return nil
		}
		if _, ok := extOK[strings.ToLower(filepath.Ext(path))]; !ok {
			return nil
		}
		walked = append(walked, path)
		return nil
	})
	rep.WalkedFiles = len(walked)
	live := map[string]struct{}{}
	for _, path := range walked {
		rel, err := filepath.Rel(rootAbs, path)
		if err != nil {
			rel = path
		}
		live[rel] = struct{}{}
		info, err := os.Stat(path)
		if err != nil {
			continue
		}
		st := FileStamp{Size: info.Size(), MtimeNs: info.ModTime().UnixNano()}
		if old, ok := idx.fileStamps[rel]; ok && StampEqual(old, st) {
			rep.StampMatch++
		} else if _, ok := idx.files[rel]; ok {
			rep.StampMismatch++
		} else {
			rep.MissingInIndex++
		}
	}
	for f := range idx.files {
		if _, ok := live[f]; !ok {
			rep.DeletedOnDisk++
		}
	}
	rep.Fresh = rep.StampMismatch == 0 && rep.MissingInIndex == 0 && rep.DeletedOnDisk == 0
	rep.DurationMS = time.Since(t0).Milliseconds()
	return rep
}

// IngestPaths re-indexes only the given relative paths (SCM ingest_paths analogue).
func (idx *Index) IngestPaths(root string, rels []string) (changed int, err error) {
	if idx == nil {
		return 0, nil
	}
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return 0, err
	}
	ignores, err := repoignore.Load(rootAbs)
	if err != nil {
		return 0, err
	}
	if idx.filePostings == nil {
		idx.filePostings = map[string]map[string]int{}
	}
	if idx.fileStamps == nil {
		idx.fileStamps = map[string]FileStamp{}
	}
	if idx.fileHashes == nil {
		idx.fileHashes = map[string]string{}
	}
	for _, rel := range rels {
		rel = strings.TrimSpace(rel)
		if rel == "" {
			continue
		}
		// Normalize and reject path traversal outside rootAbs.
		rel = filepath.Clean(rel)
		if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			continue
		}
		abs := filepath.Join(rootAbs, rel)
		abs, err = filepath.Abs(abs)
		if err != nil {
			continue
		}
		// Ensure abs is rootAbs or a descendant (prefix check with separator).
		if abs != rootAbs && !strings.HasPrefix(abs, rootAbs+string(filepath.Separator)) {
			continue
		}
		info, err := os.Stat(abs)
		if err != nil {
			// deleted
			delete(idx.files, rel)
			delete(idx.filePostings, rel)
			delete(idx.fileDefs, rel)
			delete(idx.fileRefs, rel)
			delete(idx.fileImps, rel)
			delete(idx.fileHashes, rel)
			delete(idx.fileStamps, rel)
			delete(idx.fileEdges, rel)
			idx.graph = nil
			changed++
			continue
		}
		if info.IsDir() || ignores.Ignored(rel, false) {
			continue
		}
		// os.Stat resolves a symlink before indexableFile sees it, so the
		// symlink half of that check does nothing here. Lstat the entry
		// directly: this is the path a review showed disagreeing with CrawlDir,
		// where ingesting a symlinked file added it and the next full crawl
		// silently removed it again.
		if lst, lerr := os.Lstat(abs); lerr != nil || !indexableFile(lst) {
			continue
		}
		if !indexableFile(info) {
			continue
		}
		if _, ok := extOK[strings.ToLower(filepath.Ext(abs))]; !ok {
			continue
		}
		raw, err := os.ReadFile(abs)
		if err != nil {
			continue
		}
		// Remove old postings contribution by replacing file maps then rebuild.
		loc := newLocalIndex()
		loc.add(rel, string(raw), len(raw))
		h := FileHash(raw)
		st := FileStamp{Size: info.Size(), MtimeNs: info.ModTime().UnixNano()}
		// Drop old file entry first.
		delete(idx.filePostings, rel)
		delete(idx.fileDefs, rel)
		delete(idx.fileRefs, rel)
		delete(idx.fileImps, rel)
		delete(idx.fileEdges, rel)
		idx.graph = nil
		loc.hashes[rel] = h
		loc.stamps[rel] = st
		loc.mergeInto(idx)
		idx.fileHashes[rel] = h
		idx.fileStamps[rel] = st
		changed++
	}
	rebuildGlobalFromFiles(idx)
	return changed, nil
}

// SafeRootPath resolves a relative indexed path only when it remains within root.
func SafeRootPath(root, rel string) (string, bool) {
	return safeRootPath(root, rel)
}

func safeRootPath(root, rel string) (string, bool) {
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return "", false
	}
	rel = filepath.Clean(filepath.FromSlash(rel))
	if filepath.IsAbs(rel) || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", false
	}
	abs, err := filepath.Abs(filepath.Join(rootAbs, rel))
	if err != nil || (abs != rootAbs && !strings.HasPrefix(abs, rootAbs+string(filepath.Separator))) {
		return "", false
	}
	resolvedRoot, err := filepath.EvalSymlinks(rootAbs)
	if err != nil {
		return "", false
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil || (resolved != resolvedRoot && !strings.HasPrefix(resolved, resolvedRoot+string(filepath.Separator))) {
		return "", false
	}
	return resolved, true
}

func previewFile(abs string, maxLines int) (string, int) {
	raw, err := os.ReadFile(abs)
	if err != nil {
		return "", 0
	}
	lines := strings.Split(string(raw), "\n")
	if maxLines > len(lines) {
		maxLines = len(lines)
	}
	// Prefer a non-empty early window.
	start := 0
	for i, ln := range lines {
		if strings.TrimSpace(ln) != "" {
			start = i
			break
		}
	}
	end := start + maxLines
	if end > len(lines) {
		end = len(lines)
	}
	return strings.Join(lines[start:end], "\n"), start + 1
}

func uniqueStrings(xs []string) []string {
	seen := map[string]struct{}{}
	out := make([]string, 0, len(xs))
	for _, x := range xs {
		x = strings.TrimSpace(x)
		if x == "" {
			continue
		}
		if _, ok := seen[x]; ok {
			continue
		}
		seen[x] = struct{}{}
		out = append(out, x)
	}
	return out
}
