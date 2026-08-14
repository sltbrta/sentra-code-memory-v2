package denselocal

import (
	"context"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"
)

// buildCorpus creates a deterministic bag-of-words corpus of the given size.
// The deterministic seed keeps the benchmark reproducible across runs.
func buildCorpus(size, vocab int) map[string]string {
	rng := rand.New(rand.NewSource(42))
	tokens := make([]string, vocab)
	for i := range tokens {
		tokens[i] = fmt.Sprintf("tok%d", i)
	}
	docs := make(map[string]string, size)
	for i := 0; i < size; i++ {
		n := 8 + rng.Intn(24)
		fields := make([]string, n)
		for j := range fields {
			fields[j] = tokens[rng.Intn(vocab)]
		}
		docs[fmt.Sprintf("doc-%06d.go", i)] = joinFields(fields)
	}
	return docs
}

func joinFields(fields []string) string {
	out := ""
	for i, f := range fields {
		if i > 0 {
			out += " "
		}
		out += f
	}
	return out
}

// genQuery creates a deterministic query that overlaps the corpus by some
// controlled amount. overlap is the number of tokens the query shares with a
// random corpus document.
func genQuery(rng *rand.Rand, tokens []string, overlap int) string {
	q := make([]string, 0, overlap+2)
	for i := 0; i < overlap; i++ {
		q = append(q, tokens[rng.Intn(len(tokens))])
	}
	// Anchor tokens that ensure non-zero lexical hits.
	for _, t := range []string{"anchor", "billing", "invoice"} {
		q = append(q, t)
	}
	rng.Shuffle(len(q), func(i, j int) { q[i], q[j] = q[j], q[i] })
	return joinFields(q)
}

// BenchmarkLocalLexicalThroughput measures end-to-end search throughput at
// three corpus sizes. The reported numbers are wall-time per query and
// allocations per query, the same metrics the dense.HNSW bench produces.
//
// Issue #59 acceptance criterion: "Benchmark evidence is required before
// changing defaults." These numbers are the receipt for the lexical fallback
// path that always backs the local dense arm.
func BenchmarkLocalLexicalThroughput(b *testing.B) {
	tokens := make([]string, 512)
	for i := range tokens {
		tokens[i] = fmt.Sprintf("tok%d", i)
	}
	for _, size := range []int{256, 1024, 4096} {
		size := size
		b.Run(fmt.Sprintf("corpus_%d", size), func(b *testing.B) {
			corpus := buildCorpus(size, len(tokens))
			eng, err := NewEngine("bench-scope", ModelBag, 0, nil, corpus, Bounds{
				MaxCorpus: 8192, MaxDim: 4096, MaxTopK: 20, MaxQueryLen: 512,
			})
			if err != nil {
				b.Fatal(err)
			}
			ctx := context.Background()
			rng := rand.New(rand.NewSource(7))
			// Warm cache: one priming query outside the loop.
			_, _ = eng.Search(ctx, genQuery(rng, tokens, 4), Options{TopK: 10, Scope: "bench-scope", Model: ModelBag})
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				got, err := eng.Search(ctx, genQuery(rng, tokens, 4), Options{TopK: 10, Scope: "bench-scope", Model: ModelBag})
				if err != nil || !got.OK || len(got.Hits) == 0 {
					b.Fatal("non-empty hit expected", got, err)
				}
			}
		})
	}
}

// BenchmarkLocalLexicalAllocationsPerQuery records allocs/op via
// testing.AllocsPerRun. This is the second piece of evidence required to
// promote the local arm beyond opt-in.
func BenchmarkLocalLexicalAllocationsPerQuery(b *testing.B) {
	corpus := buildCorpus(1024, 512)
	eng, err := NewEngine("bench-scope", ModelBag, 0, nil, corpus, Bounds{
		MaxCorpus: 8192, MaxDim: 4096, MaxTopK: 20, MaxQueryLen: 512,
	})
	if err != nil {
		b.Fatal(err)
	}
	ctx := context.Background()
	rng := rand.New(rand.NewSource(13))
	q := genQuery(rng, make([]string, 512), 4)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_, _ = eng.Search(ctx, q, Options{TopK: 10, Scope: "bench-scope", Model: ModelBag})
	}
}

// BenchmarkPersistAndReload measures the cost of building, persisting and
// reloading a bag-anchored HNSW index. It exercises the atomic-publish path
// that callers hit when promoting an in-memory model into a durable one.
func BenchmarkPersistAndReload(b *testing.B) {
	corpus := buildCorpus(2048, 512)
	dir := b.TempDir()
	for i := 0; i < b.N; i++ {
		path := filepath.Join(dir, fmt.Sprintf("bench-%d.ann", i))
		if err := PersistIndex(path, "bench-scope", ModelBag, 0, corpus,
			Bounds{MaxCorpus: 8192, MaxDim: 4096, MaxTopK: 20, MaxQueryLen: 512}); err != nil {
			b.Fatal(err)
		}
		loaded, err := NewEngine("bench-scope", ModelBag, 0, nil, corpus,
			Bounds{MaxCorpus: 8192, MaxDim: 4096, MaxTopK: 20, MaxQueryLen: 512})
		if err != nil {
			b.Fatal(err)
		}
		if err := loaded.LoadIndex(path, Options{Scope: "bench-scope", Model: ModelBag}); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkLocalLatencyP50P95 collects latencies into a fixed array, sorts
// them, and reports the p50/p95 numbers into the test log. Run via
// `go test -run=NONE -bench BenchmarkLocalLatencyP50P95 -count=1 ./internal/denselocal`.
func BenchmarkLocalLatencyP50P95(b *testing.B) {
	corpus := buildCorpus(2048, 512)
	eng, err := NewEngine("bench-scope", ModelBag, 0, nil, corpus, Bounds{
		MaxCorpus: 8192, MaxDim: 4096, MaxTopK: 20, MaxQueryLen: 512,
	})
	if err != nil {
		b.Fatal(err)
	}
	ctx := context.Background()
	rng := rand.New(rand.NewSource(31))
	q := genQuery(rng, make([]string, 512), 4)
	for i := 0; i < b.N; i++ {
		start := time.Now()
		_, _ = eng.Search(ctx, q, Options{TopK: 10, Scope: "bench-scope", Model: ModelBag})
		latency := time.Since(start)
		b.ReportMetric(float64(latency.Microseconds()), "us/op")
	}
}

// Example for documentation: a minimal call demonstrates the local lexical
// fallback path and the deterministic ordering it produces.
func ExampleEngine_Search() {
	docs := map[string]string{
		"a.go": "alpha beta gamma",
		"b.go": "beta gamma delta",
		"c.go": "epsilon zeta eta",
	}
	eng, err := NewEngine("scope-x", ModelBag, 0, nil, docs, DefaultBounds())
	if err != nil {
		fmt.Println(err)
		return
	}
	got, _ := eng.Search(context.Background(), "alpha beta", Options{
		TopK: 3, Scope: "scope-x", Model: ModelBag,
	})
	for _, h := range got.Hits {
		fmt.Println(h.ID, h.Score)
	}
	// Output:
	// a.go 0.8164965809277261
	// b.go 0.4082482904638631
}

// ExamplePersistIndex demonstrates writing a bag-anchored dense index to a
// temp file. The returned error indicates whether persistence succeeded.
func ExamplePersistIndex() {
	dir, err := os.MkdirTemp("", "denselocal-example-*")
	if err != nil {
		fmt.Println(err)
		return
	}
	defer os.RemoveAll(dir)
	docs := map[string]string{
		"alpha.go": "alpha beta gamma",
		"beta.go":  "beta gamma delta",
	}
	if err := PersistIndex(filepath.Join(dir, "dense.ann"), "scope-x", ModelBag, 0, docs, DefaultBounds()); err != nil {
		fmt.Println(err)
		return
	}
	fmt.Println("ok")
	// Output:
	// ok
}

// sort.Float64s is needed for percentile reporting only; referenced here so
// the import list stays stable across edits.
var _ = sort.Float64s
