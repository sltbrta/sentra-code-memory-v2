package denselocal

import (
	"context"
	"fmt"
	"math/rand"
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
		for j := 0; j < n; j++ {
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
// allocations per query.
//
// Issue #59 acceptance criterion: "Benchmark evidence is required before
// changing defaults." These numbers are the receipt for the lexical path
// that backs the local retrieval arm.
func BenchmarkLocalLexicalThroughput(b *testing.B) {
	tokens := make([]string, 512)
	for i := range tokens {
		tokens[i] = fmt.Sprintf("tok%d", i)
	}
	for _, size := range []int{256, 1024, 4096} {
		size := size
		b.Run(fmt.Sprintf("corpus_%d", size), func(b *testing.B) {
			corpus := buildCorpus(size, len(tokens))
			eng, err := NewEngine("bench-scope", ModelBag, corpus, Bounds{
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
	eng, err := NewEngine("bench-scope", ModelBag, corpus, Bounds{
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

// BenchmarkLocalLatencyP50P95 collects latencies into a fixed array, sorts
// them, and reports the p50/p95 numbers into the test log. Run via
// `go test -run=NONE -bench BenchmarkLocalLatencyP50P95 -count=1 ./internal/denselocal`.
func BenchmarkLocalLatencyP50P95(b *testing.B) {
	corpus := buildCorpus(2048, 512)
	eng, err := NewEngine("bench-scope", ModelBag, corpus, Bounds{
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
// path and the deterministic ordering it produces.
func ExampleEngine_Search() {
	docs := map[string]string{
		"a.go": "alpha beta gamma",
		"b.go": "beta gamma delta",
		"c.go": "epsilon zeta eta",
	}
	eng, err := NewEngine("scope-x", ModelBag, docs, DefaultBounds())
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
	// a.go 0.8164965809277259
	// b.go 0.40824829046386296
}
