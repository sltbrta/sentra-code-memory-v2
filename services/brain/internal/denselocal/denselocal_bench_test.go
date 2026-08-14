package denselocal

import (
	"context"
	"fmt"
	"math/rand"
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

// benchTokens returns the shared deterministic vocabulary every benchmark
// builds its corpus and queries from, so query overlap tokens can actually
// hit the corpus (a disjoint vocabulary produces a degenerate zero-hit
// query that measures the dot==0 fast path instead of real ranking work).
func benchTokens(n int) []string {
	tokens := make([]string, n)
	for i := range tokens {
		tokens[i] = fmt.Sprintf("tok%d", i)
	}
	return tokens
}

// genQuery creates a deterministic query that overlaps the corpus by some
// controlled amount. overlap is the number of tokens the query shares with
// the corpus vocabulary (these provide the non-zero lexical hits). The
// anchor tokens are deliberately out-of-vocabulary: they exercise the
// tokenize-and-miss path every real query also pays for.
func genQuery(rng *rand.Rand, tokens []string, overlap int) string {
	q := make([]string, 0, overlap+2)
	for i := 0; i < overlap; i++ {
		q = append(q, tokens[rng.Intn(len(tokens))])
	}
	// Out-of-vocabulary anchors (not present in any benchTokens corpus).
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
	tokens := benchTokens(512)
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
// b.ReportAllocs. The query is drawn from the corpus vocabulary (plus
// out-of-vocabulary anchors) and a priming query runs before the timer
// starts, so the measurement is the steady-state per-query work of a
// hitting query — not the degenerate zero-hit fast path and not one-time
// setup. A zero-hit result fails the benchmark: it means the query no
// longer overlaps the corpus and the numbers would be meaningless.
func BenchmarkLocalLexicalAllocationsPerQuery(b *testing.B) {
	tokens := benchTokens(512)
	corpus := buildCorpus(1024, len(tokens))
	eng, err := NewEngine("bench-scope", ModelBag, corpus, Bounds{
		MaxCorpus: 8192, MaxDim: 4096, MaxTopK: 20, MaxQueryLen: 512,
	})
	if err != nil {
		b.Fatal(err)
	}
	ctx := context.Background()
	rng := rand.New(rand.NewSource(13))
	q := genQuery(rng, tokens, 4)
	opts := Options{TopK: 10, Scope: "bench-scope", Model: ModelBag}
	// Prime once so first-call effects stay out of the measurement.
	prime, err := eng.Search(ctx, q, opts)
	if err != nil || !prime.OK || len(prime.Hits) == 0 {
		b.Fatalf("priming query must hit the corpus (hits=%d, err=%v)", len(prime.Hits), err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		got, err := eng.Search(ctx, q, opts)
		if err != nil || !got.OK || len(got.Hits) == 0 {
			b.Fatalf("non-empty hit expected (hits=%d, err=%v)", len(got.Hits), err)
		}
	}
}

// BenchmarkLocalLatencyP50P95 measures per-query latency for a hitting
// query over a 2048-doc corpus and reports the nearest-rank p50 and p95
// as p50-us/op and p95-us/op. Latencies are sampled inside the loop and
// percentiles are computed after the timer stops, so engine construction
// and the statistics never contaminate the samples. Run via
// `go test -run=NONE -bench BenchmarkLocalLatencyP50P95 -count=1 ./internal/denselocal`.
func BenchmarkLocalLatencyP50P95(b *testing.B) {
	tokens := benchTokens(512)
	corpus := buildCorpus(2048, len(tokens))
	eng, err := NewEngine("bench-scope", ModelBag, corpus, Bounds{
		MaxCorpus: 8192, MaxDim: 4096, MaxTopK: 20, MaxQueryLen: 512,
	})
	if err != nil {
		b.Fatal(err)
	}
	ctx := context.Background()
	rng := rand.New(rand.NewSource(31))
	q := genQuery(rng, tokens, 4)
	opts := Options{TopK: 10, Scope: "bench-scope", Model: ModelBag}
	// Prime once so first-call effects stay out of the samples.
	prime, err := eng.Search(ctx, q, opts)
	if err != nil || !prime.OK || len(prime.Hits) == 0 {
		b.Fatalf("priming query must hit the corpus (hits=%d, err=%v)", len(prime.Hits), err)
	}
	latencies := make([]time.Duration, 0, 4096)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		start := time.Now()
		got, err := eng.Search(ctx, q, opts)
		latency := time.Since(start)
		if err != nil || !got.OK || len(got.Hits) == 0 {
			b.Fatalf("non-empty hit expected (hits=%d, err=%v)", len(got.Hits), err)
		}
		latencies = append(latencies, latency)
	}
	b.StopTimer()
	sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
	percentile := func(p float64) time.Duration {
		idx := int(p * float64(len(latencies)))
		if idx >= len(latencies) {
			idx = len(latencies) - 1
		}
		return latencies[idx]
	}
	// Suppress the default ns/op (it would mix setup into the number) and
	// report the sampled percentiles instead.
	b.ReportMetric(0, "ns/op")
	b.ReportMetric(float64(percentile(0.50).Microseconds()), "p50-us/op")
	b.ReportMetric(float64(percentile(0.95).Microseconds()), "p95-us/op")
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
