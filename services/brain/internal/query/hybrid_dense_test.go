package query

import (
	"context"
	"reflect"
	"testing"

	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/ingestion"
)

func TestBagOfWordsDenseSearchRanksRelevant(t *testing.T) {
	const gen = "gen-1"
	dense := NewBagOfWordsDense(gen, map[string]string{
		"billing.go":   "package billing\nfunc InvoiceTotal() int { return 42 }",
		"auth.go":      "package auth\nfunc Login() error { return nil }",
		"unrelated.md": "the weather is sunny today",
	})
	got := dense.Search(context.Background(), gen, "invoice billing total", 2)
	if len(got) == 0 {
		t.Fatal("expected at least one dense hit")
	}
	if got[0] != "billing.go" {
		t.Fatalf("top hit = %q, want billing.go; full=%v", got[0], got)
	}
	got1 := dense.Search(context.Background(), gen, "invoice billing total", 1)
	if !reflect.DeepEqual(got1, []string{"billing.go"}) {
		t.Fatalf("topK=1 = %v, want [billing.go]", got1)
	}
}

func TestBagOfWordsDenseUnknownGenerationAndEmpty(t *testing.T) {
	dense := NewBagOfWordsDense("g", map[string]string{"a.go": "alpha beta"})
	if got := dense.Search(context.Background(), "other", "alpha", 5); got != nil {
		t.Fatalf("unknown gen = %v, want nil", got)
	}
	if got := dense.Search(context.Background(), "g", "   ", 5); got != nil {
		t.Fatalf("empty query = %v, want nil", got)
	}
	if got := dense.Search(context.Background(), "g", "zzzz-no-overlap", 5); got != nil {
		t.Fatalf("no overlap = %v, want nil", got)
	}
	var nilDense *BagOfWordsDense
	if got := nilDense.Search(context.Background(), "g", "alpha", 5); got != nil {
		t.Fatalf("nil receiver = %v, want nil", got)
	}
}

func TestLexicalCandidateRerankerOrdersByBodyOverlap(t *testing.T) {
	r := NewLexicalCandidateReranker()
	paths := []string{"weak.go", "strong.go", "mid.go"}
	bodies := map[string]string{
		"weak.go":   "package x\nfunc A() {}",
		"strong.go": "billing invoice overdue total amount",
		"mid.go":    "billing service helper",
	}
	got := r.Rerank(context.Background(), "billing invoice overdue", paths, bodies, 3)
	if len(got) != 3 {
		t.Fatalf("len = %d, want 3: %v", len(got), got)
	}
	if got[0] != "strong.go" {
		t.Fatalf("top = %q, want strong.go: %v", got[0], got)
	}
	if got[1] != "mid.go" {
		t.Fatalf("second = %q, want mid.go: %v", got[1], got)
	}
}

func TestLexicalCandidateRerankerEmptyAndTopN(t *testing.T) {
	r := NewLexicalCandidateReranker()
	if got := r.Rerank(context.Background(), "q", nil, nil, 5); got != nil {
		t.Fatalf("nil paths = %v, want nil", got)
	}
	paths := []string{"a.go", "b.go"}
	bodies := map[string]string{"a.go": "alpha beta", "b.go": "gamma"}
	got := r.Rerank(context.Background(), "alpha beta gamma", paths, bodies, 1)
	if len(got) != 1 {
		t.Fatalf("topN=1 len = %d: %v", len(got), got)
	}
}

func TestRebuildCandidatesFromPathsDenseOnly(t *testing.T) {
	existing := []candidate{
		{path: "lex.go", definitions: []string{"Lex"}, degraded: false},
		{path: "deg.go", definitions: nil, degraded: true},
	}
	fused := []string{"dense-only.go", "lex.go", "deg.go"}
	got := rebuildCandidatesFromPaths(existing, fused)
	if len(got) != 3 {
		t.Fatalf("len = %d, want 3: %#v", len(got), got)
	}
	if got[0].path != "dense-only.go" || got[0].degraded || len(got[0].definitions) != 0 {
		t.Fatalf("dense-only must be path-only non-degraded: %#v", got[0])
	}
	if got[1].path != "lex.go" || got[1].definitions[0] != "Lex" {
		t.Fatalf("lex metadata lost: %#v", got[1])
	}
	if !got[2].degraded {
		t.Fatalf("degraded flag lost: %#v", got[2])
	}
}

func TestExpandHybridDenseFusesDenseOnlyHits(t *testing.T) {
	const gen = "gen-hybrid"
	dense := NewBagOfWordsDense(gen, map[string]string{
		"billing.go": "invoice total billing amount due",
		"weak.go":    "package weak\nfunc X() {}",
		"noise.go":   "lorem ipsum dolor",
	})
	eng := &Engine{
		dense:  dense,
		limits: DefaultLimits(),
	}
	eng.limits.MaxCandidates = 4

	base := []candidate{
		{path: "weak.go", definitions: []string{"X"}, degraded: false},
	}
	snap := Snapshot{GenerationID: gen}
	got := eng.expandHybridDense(context.Background(), snap, "invoice billing total", base)
	if len(got) == 0 {
		t.Fatal("expected fused candidates")
	}
	foundBilling := false
	for _, c := range got {
		if c.path == "billing.go" {
			foundBilling = true
			if c.degraded || len(c.definitions) != 0 {
				t.Fatalf("dense-only billing must be path-only non-degraded: %#v", c)
			}
		}
		if c.path == "weak.go" && (len(c.definitions) != 1 || c.definitions[0] != "X") {
			t.Fatalf("lexical metadata lost: %#v", c)
		}
	}
	if !foundBilling {
		t.Fatalf("billing.go missing after dense fusion: %#v", got)
	}
}

func TestExpandHybridDenseWithRerank(t *testing.T) {
	const gen = "gen-rr"
	bodies := map[string]string{
		"a.go": "alpha only filler",
		"b.go": "billing invoice overdue total",
		"c.go": "misc content",
	}
	dense := NewBagOfWordsDense(gen, bodies)
	fakeRerank := &recordingReranker{order: []string{"b.go", "a.go", "c.go"}}
	eng := &Engine{
		dense:    dense,
		reranker: fakeRerank,
		limits:   DefaultLimits(),
	}
	eng.limits.MaxCandidates = 3
	snap := Snapshot{
		GenerationID: gen,
		Projection: ProjectionView{
			State: ProjectionReady,
			Files: makeTestBodies(bodies),
		},
	}
	base := []candidate{{path: "a.go", definitions: []string{"A"}}}
	got := eng.expandHybridDense(context.Background(), snap, "billing invoice", base)
	if !fakeRerank.called {
		t.Fatal("expected reranker to be invoked when bodies present")
	}
	if len(got) == 0 {
		t.Fatal("empty after hybrid expand")
	}
	if got[0].path != "b.go" {
		t.Fatalf("after rerank top = %q, want b.go: %#v", got[0].path, got)
	}
	if len(got) > eng.limits.MaxCandidates {
		t.Fatalf("len %d exceeds MaxCandidates %d", len(got), eng.limits.MaxCandidates)
	}
}

func TestExpandHybridDenseNilPortsNoop(t *testing.T) {
	eng := &Engine{limits: DefaultLimits()}
	base := []candidate{{path: "a.go", definitions: []string{"A"}}}
	got := eng.expandHybridDense(context.Background(), Snapshot{GenerationID: "g"}, "q", base)
	if !reflect.DeepEqual(got, base) {
		t.Fatalf("nil ports changed candidates: %#v", got)
	}
}

func TestNewEngineAcceptsNilDenseAndReranker(t *testing.T) {
	eng, err := NewEngine(Config{
		Corpus:                        &fixtureCorpus{snapshots: map[string]Snapshot{}},
		Authorizer:                    &stubAuthorizer{epoch: 1},
		Synthesizer:                   NewDeterministicSynthesizer(),
		Clock:                         stubClock{now: testNow},
		Limits:                        DefaultLimits(),
		Dense:                         nil,
		Reranker:                      nil,
		AllowLegacyUnadmittedEvidence: true,
	})
	if err != nil {
		t.Fatalf("NewEngine with nil Dense/Reranker: %v", err)
	}
	if eng.dense != nil || eng.reranker != nil {
		t.Fatalf("expected nil optional ports, got dense=%v reranker=%v", eng.dense, eng.reranker)
	}
}

func TestLexicalCandidateRerankerEndToEndWithBodies(t *testing.T) {
	// Full path: BagOfWordsDense + LexicalCandidateReranker through expandHybridDense.
	const gen = "gen-e2e"
	bodies := map[string]string{
		"weak.go":   "package weak\nfunc W() {}",
		"strong.go": "billing invoice overdue payment total amount",
		"mid.go":    "billing helper utilities",
	}
	eng := &Engine{
		dense:    NewBagOfWordsDense(gen, bodies),
		reranker: NewLexicalCandidateReranker(),
		limits:   DefaultLimits(),
	}
	eng.limits.MaxCandidates = 3
	snap := Snapshot{
		GenerationID: gen,
		Projection: ProjectionView{
			State: ProjectionReady,
			Files: makeTestBodies(bodies),
		},
	}
	base := []candidate{{path: "weak.go", definitions: []string{"W"}}}
	got := eng.expandHybridDense(context.Background(), snap, "billing invoice overdue", base)
	if len(got) == 0 {
		t.Fatal("expected hybrid candidates")
	}
	if got[0].path != "strong.go" {
		t.Fatalf("lexical CE top = %q, want strong.go: paths=%v", got[0].path, candidatePaths(got))
	}
}

// makeTestBodies builds minimal HydratedFile map for snapshotBodies tests.
func makeTestBodies(bodies map[string]string) map[string]ingestion.HydratedFile {
	out := make(map[string]ingestion.HydratedFile, len(bodies))
	for path, text := range bodies {
		out[path] = ingestion.HydratedFile{Content: []byte(text)}
	}
	return out
}

// recordingReranker returns a fixed order for hybrid expand tests.
type recordingReranker struct {
	order  []string
	called bool
}

func (r *recordingReranker) Rerank(_ context.Context, _ string, paths []string, _ map[string]string, topN int) []string {
	r.called = true
	if topN <= 0 || topN > len(r.order) {
		topN = len(r.order)
	}
	have := make(map[string]bool, len(paths))
	for _, p := range paths {
		have[p] = true
	}
	out := make([]string, 0, topN)
	for _, p := range r.order {
		if !have[p] {
			continue
		}
		out = append(out, p)
		if len(out) >= topN {
			break
		}
	}
	return out
}
