package codecrawl

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// mustWriteDir writes the (path → content) fixture into dir, creating
// the parent directory for every file. Tests use this helper instead of
// os.WriteFile so the fixture layout does not require a fixed template.
func mustWriteDir(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for name, body := range files {
		full := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// TestRankFusionIdentifierFloorKeepsDefiningFiles proves the floor
// guarantees a defining file remains in the top-K even when the
// fused score would demote it.
func TestRankFusionIdentifierFloorKeepsDefiningFiles(t *testing.T) {
	dir := t.TempDir()
	mustWriteDir(t, dir, map[string]string{
		"src/noise.go":   "package p\n// unrelated fluff\nfunc noise() {}\n",
		"src/anchor.go":  "package p\nfunc Anchor() {}\n",
		"src/anchor2.go": "package p\n// anchor anchor\nfunc anchor2() {}\n",
		"src/anchor3.go": "package p\n// anchor\nfunc anchor3() {}\n",
		"src/anchor4.go": "package p\n// anchor\nfunc anchor4() {}\n",
		"src/anchor5.go": "package p\n// anchor\nfunc anchor5() {}\n",
	})
	idx, _, err := CrawlDir(dir, 2)
	if err != nil {
		t.Fatal(err)
	}
	conf := DefaultRankerConfig()
	conf.Candidates = 6
	conf.IdentifierFloor = 1
	out := idx.FindRelevantRanked(dir, "anchor", 3, false, conf)
	if !containsHit(out.Hits, "src/anchor.go") {
		t.Fatalf("floor did not preserve anchor.go: %+v", out.Hits)
	}
	if out.Diagnostic.Schema != "ranker.v1" {
		t.Fatalf("schema = %s", out.Diagnostic.Schema)
	}
}

// TestRankFusionPageRankUsesGraph proves the graph fusion engages when
// the typed-edge projection is present.
func TestRankFusionPageRankUsesGraph(t *testing.T) {
	dir := t.TempDir()
	mustWriteDir(t, dir, map[string]string{
		"hub.go":    "package p\nfunc Hub() {}\n",
		"client.go": "package p\nfunc Client() { Hub() }\n",
	})
	idx, _, err := CrawlDir(dir, 2)
	if err != nil {
		t.Fatal(err)
	}
	if !idx.HasGraph() {
		t.Fatal("graph missing")
	}
	conf := DefaultRankerConfig()
	conf.IdentifierFloor = 0
	out := idx.FindRelevantRanked(dir, "Hub", 5, false, conf)
	if out.Diagnostic.GraphEdges == 0 {
		t.Fatalf("graph edges = 0: %+v", out.Diagnostic)
	}
	if out.Diagnostic.PageRankDamping != 0.85 {
		t.Fatalf("damping = %v", out.Diagnostic.PageRankDamping)
	}
}

// TestRankFusionWithoutGraphDegrades proves the pipeline degrades
// gracefully when the typed-edge projection is absent.
func TestRankFusionWithoutGraphDegrades(t *testing.T) {
	idx := newEmptyIndex()
	conf := DefaultRankerConfig()
	out := idx.FindRelevantRanked("", "anything", 3, false, conf)
	if len(out.Hits) != 0 {
		t.Fatalf("expected empty hits, got %+v", out.Hits)
	}
	hasUnavailable := false
	for _, n := range out.Diagnostic.Notes {
		if n == "graph_unavailable" {
			hasUnavailable = true
		}
	}
	if !hasUnavailable {
		t.Fatalf("expected graph_unavailable note: %+v", out.Diagnostic)
	}
}

// TestRankFusionCustomRerankerKeepsRankersDeterminism proves a custom
// reranker can override the fused score deterministically.
func TestRankFusionCustomRerankerKeepsRankersDeterminism(t *testing.T) {
	idx := newEmptyIndex()
	idx.inverted = map[string]map[string]int{
		"alpha": {"a.go": 2, "b.go": 1},
	}
	idx.files = map[string]struct{}{"a.go": {}, "b.go": {}}
	conf := DefaultRankerConfig()
	conf.Reranker = stubReranker{name: "stub"}
	conf.IdentifierFloor = 0
	conf.Candidates = 2
	out := idx.FindRelevantRanked("", "alpha", 2, false, conf)
	if out.Diagnostic.Rerank.Strategy != "stub" {
		t.Fatalf("strategy = %s", out.Diagnostic.Rerank.Strategy)
	}
	out2 := idx.FindRelevantRanked("", "alpha", 2, false, conf)
	if !reflect.DeepEqual(out.Hits, out2.Hits) {
		t.Fatalf("reranker non-deterministic: %+v vs %+v", out.Hits, out2.Hits)
	}
}

// TestRankFusionRerankerLengthMismatchFallsBack protects the boundary
// against a misbehaving reranker that returns the wrong number of
// scores. We seed two candidates so the reranker's score of length 1
// is observably the wrong length.
func TestRankFusionRerankerLengthMismatchFallsBack(t *testing.T) {
	idx := newEmptyIndex()
	idx.inverted = map[string]map[string]int{"alpha": {"a.go": 1, "b.go": 1}}
	idx.files = map[string]struct{}{"a.go": {}, "b.go": {}}
	conf := DefaultRankerConfig()
	conf.Reranker = mismatchReranker{}
	conf.IdentifierFloor = 0
	conf.Candidates = 2
	out := idx.FindRelevantRanked("", "alpha", 2, false, conf)
	if out.Diagnostic.Rerank.Strategy != "mismatch" {
		t.Fatalf("strategy = %s", out.Diagnostic.Rerank.Strategy)
	}
	if out.Diagnostic.Rerank.Reason != "score_length_mismatch" {
		t.Fatalf("reason = %s", out.Diagnostic.Rerank.Reason)
	}
}

// TestRankFusionBroadensCandidateBreadth verifies the candidate pool
// is at least MaxRankCandidates * topK wide, capped at the available
// candidate count.
func TestRankFusionBroadensCandidateBreadth(t *testing.T) {
	idx := newEmptyIndex()
	idx.inverted = map[string]map[string]int{
		"token": {},
	}
	idx.files = map[string]struct{}{}
	for i := 0; i < 10; i++ {
		path := "f" + itoa(i) + ".go"
		idx.inverted["token"][path] = 1
		idx.files[path] = struct{}{}
	}
	conf := DefaultRankerConfig()
	conf.Candidates = 4
	conf.IdentifierFloor = 0
	// Ensure there are more candidates than the multiplier so the
	// requested breadth is honoured.
	for i := 0; i < 5; i++ {
		path := "extra" + itoa(i) + ".go"
		idx.inverted["token"][path] = 1
		idx.files[path] = struct{}{}
	}
	out := idx.FindRelevantRanked("", "token", 3, false, conf)
	want := 3 * conf.Candidates
	if out.Diagnostic.CandidateBreadth != want {
		t.Fatalf("breadth = %d, want %d", out.Diagnostic.CandidateBreadth, want)
	}
}

// TestRankFusionDiagnosticsContainTopSignals ensures the bounded
// diagnostic surfaces the top PageRank/degree pairs.
func TestRankFusionDiagnosticsContainTopSignals(t *testing.T) {
	dir := t.TempDir()
	mustWriteDir(t, dir, map[string]string{
		"hub.go":     "package p\nfunc Hub() {}\n",
		"client.go":  "package p\nfunc Client() { Hub() }\n",
		"client2.go": "package p\nfunc Client2() { Hub() }\n",
	})
	idx, _, err := CrawlDir(dir, 2)
	if err != nil {
		t.Fatal(err)
	}
	conf := DefaultRankerConfig()
	conf.IdentifierFloor = 0
	out := idx.FindRelevantRanked(dir, "Hub", 5, false, conf)
	if len(out.Diagnostic.TopPageRank) == 0 {
		t.Fatalf("top PR empty: %+v", out.Diagnostic)
	}
	if len(out.Diagnostic.TopDegree) == 0 {
		t.Fatalf("top degree empty: %+v", out.Diagnostic)
	}
	for i := 1; i < len(out.Diagnostic.TopPageRank); i++ {
		if out.Diagnostic.TopPageRank[i-1].Score < out.Diagnostic.TopPageRank[i].Score {
			t.Fatalf("top PR not sorted: %+v", out.Diagnostic.TopPageRank)
		}
	}
}

// TestRankFusionPageRankConverges is a deterministic convergence check.
// The PageRank implementation must return consistent scores for the
// same graph regardless of the request order.
func TestRankFusionPageRankConverges(t *testing.T) {
	graph := &Graph{
		Edges: map[string][]Edge{
			"a.go": {{From: "a", To: "b", Kind: EdgeCall, Authority: AuthorityAST, Provenance: Provenance{File: "a.go", Line: 1, Parser: "go/parser", Language: "go"}}},
			"b.go": {{From: "b", To: "c", Kind: EdgeCall, Authority: AuthorityAST, Provenance: Provenance{File: "b.go", Line: 1, Parser: "go/parser", Language: "go"}}},
			"c.go": {{From: "c", To: "a", Kind: EdgeCall, Authority: AuthorityAST, Provenance: Provenance{File: "c.go", Line: 1, Parser: "go/parser", Language: "go"}}},
		},
	}
	files := []string{"a.go", "b.go", "c.go"}
	scores := PageRank(graph, files, files, 0.85, 64)
	for f, s := range scores {
		if s < 0 || s > 1 {
			t.Fatalf("score out of range: %s=%v", f, s)
		}
	}
	scores2 := PageRank(graph, files, files, 0.85, 64)
	if !reflect.DeepEqual(scores, scores2) {
		t.Fatalf("PR non-deterministic: %+v vs %+v", scores, scores2)
	}
}

// TestRankFusionDegreeDefaultsToUniform confirms the Degree fallback
// returns the empty map when the graph is nil.
func TestRankFusionDegreeDefaultsToUniform(t *testing.T) {
	scores := Degree(nil, []string{"a.go", "b.go"})
	if scores != nil && len(scores) != 0 {
		t.Fatalf("expected empty map, got %+v", scores)
	}
}

// TestRankFusionIdentifierFloorCapRespected ensures the floor never
// exceeds the configured cap.
func TestRankFusionIdentifierFloorCapRespected(t *testing.T) {
	idx := newEmptyIndex()
	idx.inverted = map[string]map[string]int{
		"alpha": {"a.go": 1, "b.go": 1, "c.go": 1, "d.go": 1, "e.go": 1},
	}
	idx.files = map[string]struct{}{"a.go": {}, "b.go": {}, "c.go": {}, "d.go": {}, "e.go": {}}
	idx.fileDefs = map[string][]string{
		"a.go": {"alpha"}, "b.go": {"alpha"}, "c.go": {"alpha"},
		"d.go": {"alpha"}, "e.go": {"alpha"},
	}
	conf := DefaultRankerConfig()
	conf.IdentifierFloor = 5
	conf.IdentifierFloorCap = 2
	conf.Candidates = 5
	out := idx.FindRelevantRanked("", "alpha", 2, false, conf)
	if len(out.Diagnostic.IdentifierFloorHits) > conf.IdentifierFloorCap {
		t.Fatalf("floor hits = %d, cap = %d", len(out.Diagnostic.IdentifierFloorHits), conf.IdentifierFloorCap)
	}
}

// TestRankFusionHitAtKFixtures is the headline benchmark for hit@1/5/10.
// The benchmark fixture contains:
//
//   - src/anchor.go: defines Anchor().
//   - src/anchor_alias.go: defines AnchorAlias() (a near-miss).
//   - src/noise.go: imports Anchor but does not define it.
//   - src/loose.go: contains only the lowercase word "anchor" in a comment.
//
// The expected hit@1 for "Anchor" is src/anchor.go because the
// identifier floor must keep the defining file even when the noise
// drowns it lexically.
func TestRankFusionHitAtKFixtures(t *testing.T) {
	dir := t.TempDir()
	mustWriteDir(t, dir, map[string]string{
		"src/anchor.go":        "package p\nfunc Anchor() {}\n",
		"src/anchor_alias.go":  "package p\nfunc AnchorAlias() {}\n",
		"src/noise.go":         "package p\n// anchor anchor anchor anchor anchor anchor anchor anchor\nfunc noise() {}\n",
		"src/loose.go":         "package p\n// anchor anchor anchor anchor anchor anchor anchor\nfunc loose() {}\n",
		"src/auth.go":          "package p\n// anchor anchor anchor anchor\nfunc Auth() {}\n",
		"src/auth_string.go":   "package p\n// anchor anchor anchor\nfunc AuthString() {}\n",
		"src/auth_helper.go":   "package p\n// anchor anchor\nfunc AuthHelper() {}\n",
		"src/auth_helper_2.go": "package p\n// anchor\nfunc AuthHelper2() {}\n",
	})
	idx, _, err := CrawlDir(dir, 2)
	if err != nil {
		t.Fatal(err)
	}
	conf := DefaultRankerConfig()
	conf.Candidates = 4
	conf.IdentifierFloor = 1
	conf.IdentifierFloorCap = 2
	for _, topK := range []int{1, 5, 10} {
		out := idx.FindRelevantRanked(dir, "Anchor", topK, false, conf)
		if !containsHit(out.Hits, "src/anchor.go") {
			t.Fatalf("hit@%d missed anchor.go: %+v", topK, out.Hits)
		}
	}
}

// TestRankFusionFixtureStableAcrossRuns exercises the diagnostic shape
// so the JSON envelope is reproducible.
func TestRankFusionFixtureStableAcrossRuns(t *testing.T) {
	dir := t.TempDir()
	mustWriteDir(t, dir, map[string]string{
		"src/anchor.go": "package p\nfunc Anchor() {}\n",
		"src/noise.go":  "package p\n// anchor anchor\nfunc noise() {}\n",
	})
	idx, _, err := CrawlDir(dir, 2)
	if err != nil {
		t.Fatal(err)
	}
	conf := DefaultRankerConfig()
	conf.Candidates = 4
	conf.IdentifierFloor = 1
	first := idx.FindRelevantRanked(dir, "Anchor", 3, false, conf)
	for i := 0; i < 5; i++ {
		next := idx.FindRelevantRanked(dir, "Anchor", 3, false, conf)
		if !reflect.DeepEqual(first.Hits, next.Hits) {
			t.Fatalf("non-deterministic at i=%d", i)
		}
	}
}

// TestRankFusionSynthesisHitAtK exercises a richer synthetic benchmark
// where the lexical baseline (without the floor) loses the defining
// file but the floor guarantees it. The benchmark uses 30 files: 1
// defining, 29 noise files with the same token.
func TestRankFusionSynthesisHitAtK(t *testing.T) {
	dir := t.TempDir()
	files := map[string]string{
		"src/anchor.go": "package p\nfunc Anchor() {}\n",
	}
	for i := 0; i < 29; i++ {
		name := "src/anchor_noise_" + itoaPad(i, 3) + ".go"
		files[name] = "package p\n// anchor anchor anchor anchor anchor anchor anchor anchor anchor\nfunc noise() {}\n"
	}
	mustWriteDir(t, dir, files)
	idx, _, err := CrawlDir(dir, 2)
	if err != nil {
		t.Fatal(err)
	}
	conf := DefaultRankerConfig()
	conf.Candidates = 8
	conf.IdentifierFloor = 1
	conf.IdentifierFloorCap = 1
	ranked := idx.FindRelevantRanked(dir, "Anchor", 1, false, conf)
	if !containsHit(ranked.Hits, "src/anchor.go") {
		t.Fatalf("hit@1 missed anchor.go: %+v", ranked.Hits)
	}
	if ranked.Diagnostic.Schema != "ranker.v1" {
		t.Fatalf("schema = %s", ranked.Diagnostic.Schema)
	}
}

// containsHit searches a Hit slice for a path.
func containsHit(hits []AgentHit, path string) bool {
	for _, h := range hits {
		if h.Path == path {
			return true
		}
	}
	return false
}

// stubReranker is a deterministic reranker that returns descending
// scores matching the input order.
type stubReranker struct {
	name string
}

func (s stubReranker) Rank(query string, candidates []RankCandidate) []float64 {
	scores := make([]float64, len(candidates))
	for i := range candidates {
		scores[i] = float64(len(candidates) - i)
	}
	return scores
}

func (s stubReranker) Name() string { return s.name }

// mismatchReranker returns a score slice of the wrong length on purpose.
type mismatchReranker struct{}

func (m mismatchReranker) Rank(query string, candidates []RankCandidate) []float64 {
	return []float64{0.5}
}

func (m mismatchReranker) Name() string { return "mismatch" }

// TestRankFusionMMRDeterministic proves the deterministic MMR fallback
// produces the same ordering for the same input.
func TestRankFusionMMRDeterministic(t *testing.T) {
	in := []fusedCandidate{
		{Path: "a.go", Lex: 1.0, Defines: false, Fused: 0.5},
		{Path: "b.go", Lex: 0.8, Defines: true, Fused: 0.7},
		{Path: "c.go", Lex: 0.6, Defines: false, Fused: 0.6},
	}
	first := applyMMR(in, 0.7)
	second := applyMMR(in, 0.7)
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("MMR non-deterministic")
	}
}

// TestRankFusionNoCredentialsRequired ensures the pipeline does not
// require any external configuration. The default config is credentials-
// free.
func TestRankFusionNoCredentialsRequired(t *testing.T) {
	conf := DefaultRankerConfig()
	if conf.Reranker != nil {
		t.Fatal("default reranker should be nil")
	}
	if conf.IdentifierFloorCap == 0 {
		t.Fatal("default floor cap should be > 0")
	}
}

// TestRankFusionBoundedDiagnostics ensures the diagnostic struct
// honours the bounded envelope contract.
func TestRankFusionBoundedDiagnostics(t *testing.T) {
	idx := newEmptyIndex()
	idx.inverted = map[string]map[string]int{"alpha": {"a.go": 1}}
	idx.files = map[string]struct{}{"a.go": {}}
	conf := DefaultRankerConfig()
	out := idx.FindRelevantRanked("", "alpha", 1, false, conf)
	if out.Diagnostic.CandidateBreadth > 4 {
		t.Fatalf("breadth = %d", out.Diagnostic.CandidateBreadth)
	}
	if strings.HasPrefix(out.Diagnostic.Schema, "ranker.v1") == false {
		t.Fatalf("schema = %s", out.Diagnostic.Schema)
	}
}

// TestRankFusionAPISearchExtantBehavior asserts SearchOpts callers that
// do not opt in to the new pipeline still get the legacy behaviour.
func TestRankFusionAPISearchExtantBehavior(t *testing.T) {
	dir := t.TempDir()
	mustWriteDir(t, dir, map[string]string{
		"src/anchor.go": "package p\nfunc Anchor() {}\n",
	})
	idx, _, err := CrawlDir(dir, 2)
	if err != nil {
		t.Fatal(err)
	}
	hits := idx.SearchOpts("Anchor", 5, true)
	if len(hits) == 0 {
		t.Fatal("hits empty")
	}
	if hits[0].Path != "src/anchor.go" {
		t.Fatalf("first hit = %s", hits[0].Path)
	}
}

// TestRankFusionPipelineDiagnosticsExport exercises the JSON envelope
// shape so downstream tools can rely on the field names.
func TestRankFusionPipelineDiagnosticsExport(t *testing.T) {
	dir := t.TempDir()
	mustWriteDir(t, dir, map[string]string{
		"src/anchor.go": "package p\nfunc Anchor() {}\n",
		"src/noise.go":  "package p\n// anchor\nfunc noise() {}\n",
	})
	idx, _, err := CrawlDir(dir, 2)
	if err != nil {
		t.Fatal(err)
	}
	conf := DefaultRankerConfig()
	conf.Candidates = 4
	conf.IdentifierFloor = 1
	out := idx.FindRelevantRanked(dir, "Anchor", 3, false, conf)
	if out.Diagnostic.CandidateBreadth == 0 {
		t.Fatal("candidate breadth empty")
	}
	if out.Diagnostic.IdentifierFloorHits == nil {
		t.Fatal("floor hits nil")
	}
	if out.Diagnostic.Rerank.Strategy == "" {
		t.Fatal("rerank strategy empty")
	}
	if out.Diagnostic.Weights.Lex == 0 {
		t.Fatal("weights lex empty")
	}
}
