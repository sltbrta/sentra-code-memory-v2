package hosted

import (
	"context"
	"sort"
	"testing"
	"time"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// BenchmarkQualityTracingRepresentativeWorkload compares the product answer
// path with the global no-op provider against the same path with an always-on
// recording SDK tracer. Run with a fixed sample count; the checked-in receipt
// records repeated runs instead of making noisy wall-clock timing a test gate:
//
//	go test ./services/brain/internal/hosted -run '^$' \
//	  -bench '^BenchmarkQualityTracingRepresentativeWorkload$' \
//	  -benchtime=2500x -count=3
func BenchmarkQualityTracingRepresentativeWorkload(b *testing.B) {
	b.Setenv("OUROBOROS_BRAIN_LLM", "none")
	b.Setenv("OUROBOROS_BRAIN_AGENTIC", "0")
	chunks := qualityTracingBenchmarkChunks()
	baseline := qualityTracingBenchmarkClient(b, "quality-bench-baseline", chunks)
	traced := qualityTracingBenchmarkClient(b, "quality-bench-traced", chunks)
	provider := sdktrace.NewTracerProvider(sdktrace.WithSampler(sdktrace.AlwaysSample()))
	b.Cleanup(func() { _ = provider.Shutdown(context.Background()) })
	traced.SetQualityTracer(provider.Tracer(qualityInstrumentationName))
	opts := AnswerOptions{
		Question: "What are the recovery objective, escalation owner, and rollback threshold?",
		TopK:     16,
		Mode:     "light",
	}
	for i := 0; i < 20; i++ {
		qualityTracingBenchmarkAnswer(b, baseline, opts)
		qualityTracingBenchmarkAnswer(b, traced, opts)
	}

	baselineSamples := make([]int64, 0, b.N)
	tracedSamples := make([]int64, 0, b.N)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		// Alternate order to reduce drift bias while retaining paired samples.
		if i%2 == 0 {
			baselineSamples = append(baselineSamples, qualityTracingBenchmarkDuration(b, baseline, opts))
			tracedSamples = append(tracedSamples, qualityTracingBenchmarkDuration(b, traced, opts))
		} else {
			tracedSamples = append(tracedSamples, qualityTracingBenchmarkDuration(b, traced, opts))
			baselineSamples = append(baselineSamples, qualityTracingBenchmarkDuration(b, baseline, opts))
		}
	}
	b.StopTimer()
	baselineP95 := qualityDurationP95(baselineSamples)
	tracedP95 := qualityDurationP95(tracedSamples)
	overheadPct := 100 * float64(tracedP95-baselineP95) / float64(baselineP95)
	b.ReportMetric(float64(baselineP95), "baseline_p95_ns")
	b.ReportMetric(float64(tracedP95), "traced_p95_ns")
	b.ReportMetric(overheadPct, "overhead_pct")
}

func qualityTracingBenchmarkChunks() []ChunkWrite {
	chunks := make([]ChunkWrite, 24)
	for i := range chunks {
		id := docID(i)
		chunks[i] = ChunkWrite{
			ChunkID: id + "#0", DocumentID: id,
			Text: "The service recovery objective is 15 minutes. The escalation owner is Platform Reliability. " +
				"Rollback begins when the error rate exceeds two percent for five minutes. " +
				"Operators verify regional health, preserve the incident receipt, and notify the service owner.",
		}
	}
	return chunks
}

func qualityTracingBenchmarkClient(b *testing.B, brainID string, chunks []ChunkWrite) *Client {
	b.Helper()
	c := OpenMemory(brainID)
	b.Cleanup(func() { _ = c.Close() })
	ctx := context.Background()
	if err := c.EnsureSchema(ctx); err != nil {
		b.Fatal(err)
	}
	if _, err := c.BurstUpsert(ctx, c.Config().BrainID, chunks, 1); err != nil {
		b.Fatal(err)
	}
	return c
}

func qualityTracingBenchmarkAnswer(b *testing.B, c *Client, opts AnswerOptions) {
	b.Helper()
	result := c.AnswerOpts(context.Background(), opts)
	if result.Failure != "" || result.Answer == "" {
		b.Fatalf("representative answer failed: failure=%q answer=%q", result.Failure, result.Answer)
	}
}

func qualityTracingBenchmarkDuration(b *testing.B, c *Client, opts AnswerOptions) int64 {
	b.Helper()
	started := time.Now()
	qualityTracingBenchmarkAnswer(b, c, opts)
	return time.Since(started).Nanoseconds()
}

func qualityDurationP95(samples []int64) int64 {
	if len(samples) == 0 {
		return 0
	}
	ordered := append([]int64(nil), samples...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i] < ordered[j] })
	index := (95*len(ordered) + 99) / 100
	return ordered[index-1]
}
