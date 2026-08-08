package hosted

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace/noop"
)

func TestQualityTracingEmitsBoundedPipelineSpans(t *testing.T) {
	t.Setenv("OUROBOROS_BRAIN_LLM", "none")
	t.Setenv("OUROBOROS_BRAIN_AGENTIC", "0")

	c := qualityTraceClient(t, "trace-shape", []ChunkWrite{{
		ChunkID: "policy#0", DocumentID: "policy", Text: "The recovery objective is 15 minutes.",
	}})
	recorder, provider := qualitySpanRecorder(t, c)

	result := c.AnswerOpts(context.Background(), AnswerOptions{
		Question: "What is the recovery objective?", TopK: 4, Mode: "light",
	})
	if result.Failure != "" || result.Answer == "" {
		t.Fatalf("answer failed: failure=%q answer=%q", result.Failure, result.Answer)
	}
	_ = provider.ForceFlush(context.Background())

	spans := recorder.Ended()
	answer := oneQualitySpan(t, spans, answerQualitySpanName)
	for _, name := range qualityRequiredAnswerStages {
		if len(qualitySpansNamed(spans, name)) == 0 {
			t.Fatalf("required span %q missing; all=%v", name, qualitySpanNames(spans))
		}
	}

	for _, span := range spans {
		if len(span.Attributes()) > qualityMaxAttributes {
			t.Fatalf("%s has %d attrs; cap=%d", span.Name(), len(span.Attributes()), qualityMaxAttributes)
		}
		if len(span.Events()) != 0 || len(span.Links()) != 0 {
			t.Fatalf("%s must not emit content-bearing events or links", span.Name())
		}
		if span.SpanContext().TraceID() != answer.SpanContext().TraceID() {
			t.Fatalf("%s not correlated by native trace context", span.Name())
		}
		attrs := qualityAttributeMap(span.Attributes())
		if _, ok := attrs[qualityAttrComponent]; !ok {
			t.Fatalf("%s missing finite component correlation: %v", span.Name(), attrs)
		}
		for _, kv := range span.Attributes() {
			if kv.Value.Type() == attribute.STRING && len(kv.Value.AsString()) > qualityMaxStringBytes {
				t.Fatalf("%s attribute %s exceeds string cap", span.Name(), kv.Key)
			}
		}
	}
	if answer.Parent().IsValid() {
		t.Fatalf("answer unexpectedly has parent %v", answer.Parent())
	}
}

func TestQualityTracingNeverExportsContentSecretsIDsErrorsModelsOrGold(t *testing.T) {
	t.Setenv("OUROBOROS_BRAIN_LLM", "none")
	t.Setenv("OUROBOROS_BRAIN_AGENTIC", "0")

	const secret = "marker_123"
	const sourceID = "gold-secret-q7V9"
	c := qualityTraceClient(t, "private-brain-id", []ChunkWrite{{
		ChunkID: sourceID + "#0", DocumentID: sourceID,
		Text: "The private recovery value is " + secret + ".",
	}})
	recorder, provider := qualitySpanRecorder(t, c)

	result := c.AnswerOpts(context.Background(), AnswerOptions{
		Question:     "What is the private recovery value " + secret + "?",
		QuestionType: "gold-label-" + secret,
		Mode:         "mode-" + secret,
		TopK:         qualityCountMax + 500,
		SourceTypes:  []string{"source-" + secret},
		HistoryText:  "history-" + secret,
		GoldDocIDs:   []string{sourceID},
	})
	if result.Answer == "" || !strings.Contains(result.Answer, secret) {
		t.Fatalf("privacy canary did not reach behavior output: %+v", result)
	}
	_ = provider.ForceFlush(context.Background())

	for _, span := range recorder.Ended() {
		surface := span.Name() + " " + span.Status().Description
		for _, kv := range span.Attributes() {
			surface += " " + string(kv.Key) + "=" + kv.Value.Emit()
		}
		for _, forbidden := range []string{
			secret, sourceID, "private-brain-id", "gold", "source-", "history-",
			"document_id", "chunk_id", "request_id", "model",
		} {
			if strings.Contains(strings.ToLower(surface), strings.ToLower(forbidden)) {
				t.Fatalf("%s leaked forbidden surface %q: %s", span.Name(), forbidden, surface)
			}
		}
	}

	// Arbitrary diagnostics are collapsed at the typed receipt boundary.
	receipt := answerQualityReceipt(AnswerResult{
		Provider: "provider-" + secret,
		RetrievalDiagnostics: map[string]any{
			"freshness": "fresh-" + secret,
			"llm_cost":  map[string]any{"total_cost_usd": 0.000321, "raw_error": "boom-" + secret},
		},
	})
	attrs := qualityAttributeMap(qualityAttributes(receipt))
	if attrs[qualityAttrProvider] != "unknown" || attrs[qualityAttrFreshness] != "unknown" ||
		attrs[qualityAttrCostMicroUSD] != int64(321) {
		t.Fatalf("sanitized diagnostic receipt wrong: %#v", attrs)
	}
}

func TestQualityTracingMissingAndFailedStagesAreExplicit(t *testing.T) {
	t.Setenv("OUROBOROS_BRAIN_LLM", "none")
	t.Setenv("OUROBOROS_BRAIN_AGENTIC", "0")
	c := qualityTraceClient(t, "trace-failure", []ChunkWrite{{
		ChunkID: "d1#0", DocumentID: "d1", Text: "Widget color is blue.",
	}})
	recorder, provider := qualitySpanRecorder(t, c)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_ = c.AnswerOpts(ctx, AnswerOptions{Question: "What color is the widget?", TopK: 3})
	_ = provider.ForceFlush(context.Background())

	spans := recorder.Ended()
	retrieve := oneQualitySpan(t, spans, retrievalQualitySpanName)
	if retrieve.Status().Code != codes.Error ||
		qualityAttributeMap(retrieve.Attributes())[qualityAttrOutcome] != "canceled" {
		t.Fatalf("failed retrieval span not classified: status=%v attrs=%v", retrieve.Status(), retrieve.Attributes())
	}
	for _, name := range []string{rerankQualitySpanName, packingQualitySpanName, synthesisQualitySpanName, citationQualitySpanName} {
		span := oneQualitySpan(t, spans, name)
		if got := qualityAttributeMap(span.Attributes())[qualityAttrOutcome]; got != "not_run" {
			t.Fatalf("missing %s stage outcome=%v want not_run", name, got)
		}
		if span.Status().Code != codes.Unset {
			t.Fatalf("not_run %s must not claim success or failure: %v", name, span.Status())
		}
	}

	// A real ingest validation failure is an error span, but its raw error text
	// is never recorded as an event, description, or attribute.
	before := len(recorder.Ended())
	err := c.UpsertChunks(context.Background(), c.Config().BrainID, nil)
	if err == nil {
		t.Fatal("expected empty ingest to fail")
	}
	_ = provider.ForceFlush(context.Background())
	ingest := oneQualitySpan(t, recorder.Ended()[before:], ingestQualitySpanName)
	if ingest.Status().Code != codes.Error || ingest.Status().Description != "error" || len(ingest.Events()) != 0 {
		t.Fatalf("failed ingest span surface unsafe: status=%v events=%v", ingest.Status(), ingest.Events())
	}
	if strings.Contains(fmt.Sprint(ingest.Attributes()), err.Error()) {
		t.Fatal("raw ingest error leaked into attributes")
	}
}

func TestQualityTracingIngestSuccessIsBoundedAndIDFree(t *testing.T) {
	c := OpenMemory("private-ingest-brain-id")
	t.Cleanup(func() { _ = c.Close() })
	if err := c.EnsureSchema(context.Background()); err != nil {
		t.Fatal(err)
	}
	recorder, provider := qualitySpanRecorder(t, c)
	result, err := c.BurstUpsert(context.Background(), c.Config().BrainID, []ChunkWrite{{
		ChunkID: "private-chunk-id", DocumentID: "private-document-id", Text: "private content canary",
	}}, 1)
	if err != nil || result.Ingested != 1 {
		t.Fatalf("ingest failed: result=%+v err=%v", result, err)
	}
	_ = provider.ForceFlush(context.Background())
	span := oneQualitySpan(t, recorder.Ended(), ingestQualitySpanName)
	attrs := qualityAttributeMap(span.Attributes())
	if attrs[qualityAttrArm] != "burst" || attrs[qualityAttrOutcome] != "ok" ||
		attrs[qualityAttrInputCount] != int64(1) || attrs[qualityAttrOutputCount] != int64(1) ||
		attrs[qualityAttrFreshness] != "updated" {
		t.Fatalf("ingest attrs=%#v", attrs)
	}
	surface := fmt.Sprint(span.Attributes())
	for _, forbidden := range []string{"private-ingest-brain-id", "private-chunk-id", "private-document-id", "private content canary"} {
		if strings.Contains(surface, forbidden) {
			t.Fatalf("ingest span leaked %q: %s", forbidden, surface)
		}
	}
}

func TestQualityTracingSanitizedProviderCostFreshnessAndArmCorrelation(t *testing.T) {
	receipt := retrievalQualityReceipt([]Passage{{}}, map[string]any{
		"retrieve_class": "interactive_local",
		"cache_hit":      true,
		"rerank_backend": "cohere",
		"arms":           []string{"raw-id-must-not-export"},
	}, nil)
	attrs := qualityAttributeMap(qualityAttributes(receipt))
	if attrs[qualityAttrArm] != "interactive_local" || attrs[qualityAttrFreshness] != "cached" ||
		attrs[qualityAttrCacheHit] != true {
		t.Fatalf("retrieval receipt mismatch: %#v", attrs)
	}

	rerank := rerankQualityReceipt([]Passage{{}}, map[string]any{
		"rerank": "ok", "rerank_backend": "cohere", "rerank_fallback": true,
		"rerank_cohere_error": "credential and raw provider response",
	})
	rattrs := qualityAttributeMap(qualityAttributes(rerank))
	if rattrs[qualityAttrArm] != "cohere" || rattrs[qualityAttrOutcome] != "degraded" {
		t.Fatalf("rerank receipt mismatch: %#v", rattrs)
	}
	if strings.Contains(fmt.Sprint(rattrs), "credential") {
		t.Fatal("raw rerank error crossed sanitized receipt")
	}

	// Start-time mode plus finish-time cost/shape must still respect the total
	// unique-attribute cap on the recorded root span.
	recorder := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })
	c := &Client{}
	c.SetQualityTracer(provider.Tracer(qualityInstrumentationName))
	ctx, span := c.startAnswerQualitySpan(context.Background(), "bench")
	finishAnswerQualitySpan(ctx, span, AnswerResult{
		Answer: "content is never exported", Provider: "openai",
		CitedDocumentIDs: []string{"private-document-id"}, Claims: []Claim{{}},
		RetrievalDiagnostics: map[string]any{
			"passage_count": 1,
			"llm_cost":      map[string]any{"total_cost_usd": 999999.0},
		},
	})
	root := oneQualitySpan(t, recorder.Ended(), answerQualitySpanName)
	if len(root.Attributes()) != qualityMaxAttributes {
		t.Fatalf("cost-bearing root attrs=%d want cap=%d: %v", len(root.Attributes()), qualityMaxAttributes, root.Attributes())
	}
	rootAttrs := qualityAttributeMap(root.Attributes())
	if rootAttrs[qualityAttrCostMicroUSD] != qualityCostMicroUSDMax || rootAttrs[qualityAttrProvider] != "openai" {
		t.Fatalf("root sanitized cost/provider attrs=%#v", rootAttrs)
	}
}

func TestQualityTracingNoopPreservesAnswerBehavior(t *testing.T) {
	t.Setenv("OUROBOROS_BRAIN_LLM", "none")
	t.Setenv("OUROBOROS_BRAIN_AGENTIC", "0")
	chunks := []ChunkWrite{{ChunkID: "d1#0", DocumentID: "d1", Text: "Widget color is blue."}}
	baselineClient := qualityTraceClient(t, "noop-baseline", chunks)
	noopClient := qualityTraceClient(t, "noop-injected", chunks)
	noopClient.SetQualityTracer(noop.NewTracerProvider().Tracer(qualityInstrumentationName))

	opts := AnswerOptions{Question: "What color is the widget?", TopK: 3}
	baseline := baselineClient.AnswerOpts(context.Background(), opts)
	got := noopClient.AnswerOpts(context.Background(), opts)
	if baseline.Answer != got.Answer || baseline.Failure != got.Failure ||
		baseline.SearchMode != got.SearchMode || !reflect.DeepEqual(baseline.CitedDocumentIDs, got.CitedDocumentIDs) {
		t.Fatalf("no-op tracing changed answer behavior:\nbaseline=%+v\ntraced=%+v", baseline, got)
	}
	noopClient.SetQualityTracer(nil)
	_ = noopClient.AnswerOpts(nil, opts)
}

func TestQualityTracingPackingAndSynthesisCountsUseTruncatedPromptInputs(t *testing.T) {
	t.Setenv("OUROBOROS_BRAIN_LLM", "none")
	passages := make([]Passage, 0, 26)
	for i := 0; i < 20; i++ {
		passages = append(passages, Passage{
			DocumentID: docID(i), Text: "evidence " + docID(i), Score: float64(i + 1),
		})
	}
	for i := 0; i < 6; i++ {
		passages = append(passages, Passage{
			DocumentID: "turn:" + docID(i), Text: "conversation " + docID(i),
		})
	}

	recorder := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })
	c := &Client{}
	c.SetQualityTracer(provider.Tracer(qualityInstrumentationName))
	ctx, root := c.startAnswerQualitySpan(context.Background(), "light")
	if _, _, _, err := synthesizeOnce(ctx, "What is the policy?", "basic", passages, 400, "", nil, ""); err != nil {
		t.Fatal(err)
	}
	root.End()

	packAttrs := qualityAttributeMap(oneQualitySpan(t, recorder.Ended(), packingQualitySpanName).Attributes())
	if got := packAttrs[qualityAttrInputCount]; got != int64(26) {
		t.Fatalf("packing input count=%v want 26 candidates", got)
	}
	if got := packAttrs[qualityAttrOutputCount]; got != int64(20) {
		t.Fatalf("packing output count=%v want 20 emitted prompt inputs", got)
	}
	synthAttrs := qualityAttributeMap(oneQualitySpan(t, recorder.Ended(), synthesisQualitySpanName).Attributes())
	if got := synthAttrs[qualityAttrInputCount]; got != int64(20) {
		t.Fatalf("synthesis input count=%v want 20 emitted prompt inputs", got)
	}
}

func TestQualityTracingDeterministicOverheadP95StructuralReceipt(t *testing.T) {
	// Fixed structural samples measure attribute-write work, not scheduler or
	// wall-clock noise. Repeating the seven public span shapes makes p95 stable
	// on every machine while the hard cap remains enforced in production code.
	shapes := []qualityReceipt{
		{component: "answer", mode: "light", outcome: "ok", provider: "openai", freshness: "observed", costMicroUSD: 7, hasCost: true, hasAnswerShape: true},
		{component: "ingest", arm: "burst", outcome: "ok", freshness: "updated", inputCount: 4, outputCount: 4, hasInput: true, hasOutput: true},
		{component: "retrieve", arm: "interactive", outcome: "ok", freshness: "observed", inputCount: 8, outputCount: 8, hasInput: true, hasOutput: true, hasCacheHit: true},
		{component: "rerank", arm: "cohere", outcome: "ok", inputCount: 20, outputCount: 20, hasInput: true, hasOutput: true},
		{component: "packing", arm: "prompt", outcome: "ok", freshness: "observed", inputCount: 8, outputCount: 8, hasInput: true, hasOutput: true},
		{component: "synthesis", arm: "synth", outcome: "ok", provider: "openai", freshness: "observed", inputCount: 8, outputCount: 2, hasInput: true, hasOutput: true},
		{component: "citations", arm: "grounding", outcome: "grounded", freshness: "observed", groundingState: "ok", inputCount: 8, hasInput: true, hasAnswerShape: true},
	}
	samples := make([]int, 0, len(shapes)*20)
	for i := 0; i < 20; i++ {
		for _, shape := range shapes {
			samples = append(samples, len(qualityAttributes(shape)))
		}
	}
	p95 := fixedP95(samples)
	receipt := map[string]any{
		"metric": "attribute_writes_per_span", "samples": len(samples),
		"p95": p95, "threshold": qualityMaxAttributes, "wall_clock_gate": false,
	}
	encoded, _ := json.Marshal(receipt)
	t.Log(string(encoded))
	if p95 > qualityMaxAttributes {
		t.Fatalf("deterministic overhead receipt exceeds structural cap: %s", encoded)
	}
	if p95 != 9 || len(samples) != 140 {
		t.Fatalf("fixed receipt drifted; review and update evidence intentionally: %s", encoded)
	}
}

func BenchmarkQualityTracingNoopStructuralReceipt(b *testing.B) {
	receipt := qualityReceipt{
		component: "synthesis", arm: "synth", outcome: "ok", provider: "openai", freshness: "observed",
		inputCount: 8, outputCount: 2, hasInput: true, hasOutput: true,
	}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = qualityAttributes(receipt)
	}
}

func fixedP95(samples []int) int {
	if len(samples) == 0 {
		return 0
	}
	ordered := append([]int(nil), samples...)
	sort.Ints(ordered)
	index := (95*len(ordered) + 99) / 100
	if index < 1 {
		index = 1
	}
	return ordered[index-1]
}

func qualityTraceClient(t *testing.T, brainID string, chunks []ChunkWrite) *Client {
	t.Helper()
	c := OpenMemory(brainID)
	t.Cleanup(func() { _ = c.Close() })
	ctx := context.Background()
	if err := c.EnsureSchema(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := c.BurstUpsert(ctx, c.Config().BrainID, chunks, 1); err != nil {
		t.Fatal(err)
	}
	return c
}

func qualitySpanRecorder(t *testing.T, c *Client) (*tracetest.SpanRecorder, *sdktrace.TracerProvider) {
	t.Helper()
	recorder := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })
	c.SetQualityTracer(provider.Tracer(qualityInstrumentationName))
	return recorder, provider
}

func qualitySpansNamed(spans []sdktrace.ReadOnlySpan, name string) []sdktrace.ReadOnlySpan {
	out := make([]sdktrace.ReadOnlySpan, 0, len(spans))
	for _, span := range spans {
		if span.Name() == name {
			out = append(out, span)
		}
	}
	return out
}

func oneQualitySpan(t *testing.T, spans []sdktrace.ReadOnlySpan, name string) sdktrace.ReadOnlySpan {
	t.Helper()
	matches := qualitySpansNamed(spans, name)
	if len(matches) != 1 {
		t.Fatalf("%s span count=%d; all=%v", name, len(matches), qualitySpanNames(spans))
	}
	return matches[0]
}

func qualitySpanNames(spans []sdktrace.ReadOnlySpan) []string {
	names := make([]string, 0, len(spans))
	for _, span := range spans {
		names = append(names, span.Name())
	}
	return names
}

func qualityAttributeMap(attrs []attribute.KeyValue) map[string]any {
	out := make(map[string]any, len(attrs))
	for _, kv := range attrs {
		out[string(kv.Key)] = kv.Value.AsInterface()
	}
	return out
}

func TestQualityTracingFailureHelperDoesNotRecordRawError(t *testing.T) {
	recorder := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })
	_, span := provider.Tracer(qualityInstrumentationName).Start(context.Background(), synthesisQualitySpanName)
	finishSynthesisQualitySpan(span, synthRaw{}, "secret-provider-id", errors.New("raw secret error"))
	got := oneQualitySpan(t, recorder.Ended(), synthesisQualitySpanName)
	if got.Status().Code != codes.Error || got.Status().Description != "error" || len(got.Events()) != 0 {
		t.Fatalf("unsafe failure span: status=%v events=%v", got.Status(), got.Events())
	}
	if strings.Contains(fmt.Sprint(got.Attributes()), "secret") {
		t.Fatal("failure helper exported raw provider/error")
	}
}
