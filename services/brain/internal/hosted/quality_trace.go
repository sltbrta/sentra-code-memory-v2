package hosted

import (
	"context"
	"errors"
	"math"
	"strings"
	"sync"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

const (
	qualityInstrumentationName = "github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/hosted"
	qualityCountMax            = 1000
	qualityCostMicroUSDMax     = int64(1_000_000_000)
	qualityMaxAttributes       = 9
	qualityMaxStringBytes      = 64

	answerQualitySpanName    = "ouroboros.brain.answer"
	ingestQualitySpanName    = "ouroboros.brain.ingest"
	retrievalQualitySpanName = "ouroboros.brain.retrieve"
	rerankQualitySpanName    = "ouroboros.brain.rerank"
	packingQualitySpanName   = "ouroboros.brain.pack"
	synthesisQualitySpanName = "ouroboros.brain.synthesize"
	citationQualitySpanName  = "ouroboros.brain.citations"
)

const (
	qualityAttrComponent      = "ouroboros.pipeline.component"
	qualityAttrArm            = "ouroboros.pipeline.arm"
	qualityAttrMode           = "ouroboros.brain.mode"
	qualityAttrOutcome        = "ouroboros.quality.outcome"
	qualityAttrInputCount     = "ouroboros.quality.input_count"
	qualityAttrOutputCount    = "ouroboros.quality.output_count"
	qualityAttrCacheHit       = "ouroboros.retrieval.cache_hit"
	qualityAttrFreshness      = "ouroboros.evidence.freshness"
	qualityAttrProvider       = "ouroboros.provider.name"
	qualityAttrCostMicroUSD   = "ouroboros.cost.estimated_micro_usd"
	qualityAttrCitationCount  = "ouroboros.answer.citation_count"
	qualityAttrClaimCount     = "ouroboros.answer.claim_count"
	qualityAttrAbstained      = "ouroboros.answer.abstained"
	qualityAttrGroundingState = "ouroboros.grounding.status"
)

var qualityRequiredAnswerStages = []string{
	retrievalQualitySpanName,
	rerankQualitySpanName,
	packingQualitySpanName,
	synthesisQualitySpanName,
	citationQualitySpanName,
}

type qualityContextKey struct{}

// qualityStageTracker is request-local and bounded by the fixed stage set.
// It also carries the already-sanitized freshness category from retrieval to
// packing/synthesis; it never stores diagnostics maps, content, IDs, or errors.
type qualityStageTracker struct {
	mu        sync.Mutex
	seen      map[string]bool
	freshness string
}

func newQualityStageTracker() *qualityStageTracker {
	return &qualityStageTracker{seen: make(map[string]bool, len(qualityRequiredAnswerStages))}
}

func (t *qualityStageTracker) mark(name string) {
	if t == nil {
		return
	}
	t.mu.Lock()
	if qualityKnownSpanName(name) {
		t.seen[name] = true
	}
	t.mu.Unlock()
}

func (t *qualityStageTracker) missing() []string {
	if t == nil {
		return append([]string(nil), qualityRequiredAnswerStages...)
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]string, 0, len(qualityRequiredAnswerStages))
	for _, name := range qualityRequiredAnswerStages {
		if !t.seen[name] {
			out = append(out, name)
		}
	}
	return out
}

func (t *qualityStageTracker) setFreshness(value string) {
	if t == nil {
		return
	}
	t.mu.Lock()
	t.freshness = qualityFreshnessValue(value)
	t.mu.Unlock()
}

func (t *qualityStageTracker) getFreshness() string {
	if t == nil {
		return "unknown"
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return qualityFreshnessValue(t.freshness)
}

type qualityContext struct {
	tracer  trace.Tracer
	tracker *qualityStageTracker
}

// qualityReceipt is the sole attribute source. Constructors below accept
// ordinary pipeline diagnostics, collapse them to finite enums/capped numbers,
// and discard everything else before this value reaches OpenTelemetry.
type qualityReceipt struct {
	component      string
	arm            string
	mode           string
	outcome        string
	provider       string
	freshness      string
	groundingState string
	inputCount     int
	outputCount    int
	citationCount  int
	claimCount     int
	costMicroUSD   int64
	cacheHit       bool
	abstained      bool
	hasInput       bool
	hasOutput      bool
	hasCost        bool
	hasCacheHit    bool
	hasAnswerShape bool
}

// SetQualityTracer injects the OpenTelemetry API tracer used by subsequent
// operations. Configure it before serving concurrent requests. A nil tracer
// restores global-provider behavior; OpenTelemetry's unset provider is a no-op.
func (c *Client) SetQualityTracer(tracer trace.Tracer) {
	if c != nil {
		c.qualityTracer = tracer
	}
}

func (c *Client) tracer() trace.Tracer {
	if c != nil && c.qualityTracer != nil {
		return c.qualityTracer
	}
	return otel.Tracer(qualityInstrumentationName)
}

func qualityFields(ctx context.Context) qualityContext {
	if ctx == nil {
		return qualityContext{}
	}
	fields, _ := ctx.Value(qualityContextKey{}).(qualityContext)
	return fields
}

func qualityTracer(ctx context.Context) trace.Tracer {
	if fields := qualityFields(ctx); fields.tracer != nil {
		return fields.tracer
	}
	return otel.Tracer(qualityInstrumentationName)
}

func (c *Client) startAnswerQualitySpan(ctx context.Context, mode string) (context.Context, trace.Span) {
	if ctx == nil {
		ctx = context.Background()
	}
	fields := qualityContext{tracer: c.tracer(), tracker: newQualityStageTracker()}
	ctx = context.WithValue(ctx, qualityContextKey{}, fields)
	receipt := qualityReceipt{component: "answer", mode: qualityMode(mode)}
	return fields.tracer.Start(ctx, answerQualitySpanName, trace.WithAttributes(qualityAttributes(receipt)...))
}

func (c *Client) startIngestQualitySpan(ctx context.Context, arm string, input int) (context.Context, trace.Span) {
	if ctx == nil {
		ctx = context.Background()
	}
	fields := qualityFields(ctx)
	if fields.tracer == nil {
		fields.tracer = c.tracer()
		ctx = context.WithValue(ctx, qualityContextKey{}, fields)
	}
	receipt := qualityReceipt{
		component: "ingest", arm: qualityIngestArm(arm), inputCount: boundedCount(input), hasInput: true,
	}
	return fields.tracer.Start(ctx, ingestQualitySpanName, trace.WithAttributes(qualityAttributes(receipt)...))
}

func (c *Client) startRetrievalQualitySpan(ctx context.Context, requestedTopK int) (context.Context, trace.Span) {
	if ctx == nil {
		ctx = context.Background()
	}
	fields := qualityFields(ctx)
	if fields.tracer == nil {
		fields.tracer = c.tracer()
		ctx = context.WithValue(ctx, qualityContextKey{}, fields)
	}
	if fields.tracker != nil {
		fields.tracker.mark(retrievalQualitySpanName)
	}
	receipt := qualityReceipt{
		component: "retrieve", arm: "hybrid", inputCount: boundedCount(requestedTopK), hasInput: true,
	}
	return fields.tracer.Start(ctx, retrievalQualitySpanName, trace.WithAttributes(qualityAttributes(receipt)...))
}

func startRerankQualitySpan(ctx context.Context, input int) (context.Context, trace.Span) {
	if ctx == nil {
		ctx = context.Background()
	}
	fields := qualityFields(ctx)
	if fields.tracker != nil {
		fields.tracker.mark(rerankQualitySpanName)
	}
	receipt := qualityReceipt{component: "rerank", arm: "cross_encoder", inputCount: boundedCount(input), hasInput: true}
	return qualityTracer(ctx).Start(ctx, rerankQualitySpanName, trace.WithAttributes(qualityAttributes(receipt)...))
}

func startPackingQualitySpan(ctx context.Context, input int) (context.Context, trace.Span) {
	if ctx == nil {
		ctx = context.Background()
	}
	fields := qualityFields(ctx)
	if fields.tracker != nil {
		fields.tracker.mark(packingQualitySpanName)
	}
	receipt := qualityReceipt{
		component: "packing", arm: "prompt", inputCount: boundedCount(input), hasInput: true,
		freshness: fields.tracker.getFreshness(),
	}
	return qualityTracer(ctx).Start(ctx, packingQualitySpanName, trace.WithAttributes(qualityAttributes(receipt)...))
}

func startSynthesisQualitySpan(ctx context.Context, input int) (context.Context, trace.Span) {
	if ctx == nil {
		ctx = context.Background()
	}
	fields := qualityFields(ctx)
	if fields.tracker != nil {
		fields.tracker.mark(synthesisQualitySpanName)
	}
	receipt := qualityReceipt{
		component: "synthesis", arm: qualitySynthesisArm(llmStageFrom(ctx)),
		inputCount: boundedCount(input), hasInput: true, freshness: fields.tracker.getFreshness(),
	}
	return qualityTracer(ctx).Start(ctx, synthesisQualitySpanName, trace.WithAttributes(qualityAttributes(receipt)...))
}

func (c *Client) groundAnswerWithQualityTrace(
	ctx context.Context,
	question, answer string,
	cited []string,
	claims []Claim,
	passages []Passage,
	questionType string,
) (grounded Grounded) {
	if ctx == nil {
		ctx = context.Background()
	}
	fields := qualityFields(ctx)
	if fields.tracker != nil {
		fields.tracker.mark(citationQualitySpanName)
	}
	receipt := qualityReceipt{
		component: "citations", arm: "grounding", inputCount: boundedCount(len(passages)), hasInput: true,
		freshness: fields.tracker.getFreshness(),
	}
	_, span := qualityTracer(ctx).Start(ctx, citationQualitySpanName, trace.WithAttributes(qualityAttributes(receipt)...))
	defer func() { finishCitationQualitySpan(span, grounded) }()
	return groundAnswerInPassages(question, answer, cited, claims, passages, questionType)
}

func finishAnswerQualitySpan(ctx context.Context, span trace.Span, result AnswerResult) {
	if span == nil {
		return
	}
	finishMissingQualitySpans(ctx)
	receipt := answerQualityReceipt(result)
	span.SetAttributes(qualityAttributes(receipt)...)
	setQualityStatus(span, receipt.outcome, receipt.outcome == "error" || receipt.outcome == "denied")
	span.End()
}

func finishIngestQualitySpan(span trace.Span, input, output int, err error) {
	if span == nil {
		return
	}
	outcome := "ok"
	freshness := "updated"
	if err != nil {
		outcome = "error"
		freshness = "unknown"
	} else if output == 0 {
		outcome = "empty"
		freshness = "unchanged"
	} else if output < input {
		outcome = "partial"
	}
	receipt := qualityReceipt{
		component: "ingest", outcome: outcome, freshness: freshness,
		inputCount: boundedCount(input), outputCount: boundedCount(output), hasInput: true, hasOutput: true,
	}
	span.SetAttributes(qualityAttributes(receipt)...)
	setQualityStatus(span, outcome, err != nil)
	span.End()
}

func finishRetrievalQualitySpan(ctx context.Context, span trace.Span, passages []Passage, diag map[string]any, err error) {
	if span == nil {
		return
	}
	receipt := retrievalQualityReceipt(passages, diag, err)
	if tracker := qualityFields(ctx).tracker; tracker != nil {
		tracker.setFreshness(receipt.freshness)
	}
	span.SetAttributes(qualityAttributes(receipt)...)
	setQualityStatus(span, receipt.outcome, err != nil)
	span.End()
}

func finishRerankQualitySpan(span trace.Span, passages []Passage, diag map[string]any) {
	if span == nil {
		return
	}
	receipt := rerankQualityReceipt(passages, diag)
	span.SetAttributes(qualityAttributes(receipt)...)
	setQualityStatus(span, receipt.outcome, receipt.outcome == "error")
	span.End()
}

func finishPackingQualitySpan(span trace.Span, output int) {
	if span == nil {
		return
	}
	outcome := "ok"
	if output == 0 {
		outcome = "empty"
	}
	receipt := qualityReceipt{component: "packing", outcome: outcome, outputCount: boundedCount(output), hasOutput: true}
	span.SetAttributes(qualityAttributes(receipt)...)
	setQualityStatus(span, outcome, false)
	span.End()
}

func finishSynthesisQualitySpan(span trace.Span, raw synthRaw, provider string, err error) {
	if span == nil {
		return
	}
	outcome := "ok"
	if errors.Is(err, context.DeadlineExceeded) {
		outcome = "timeout"
	} else if errors.Is(err, context.Canceled) {
		outcome = "canceled"
	} else if err != nil {
		outcome = "error"
	} else if strings.TrimSpace(raw.Answer) == "" {
		outcome = "empty"
	} else if looksLikeAbstention(raw.Answer) {
		outcome = "abstained"
	}
	receipt := qualityReceipt{
		component: "synthesis", outcome: outcome, provider: qualityProvider(provider),
		outputCount: boundedCount(len(raw.Claims)), hasOutput: true,
	}
	span.SetAttributes(qualityAttributes(receipt)...)
	setQualityStatus(span, outcome, err != nil)
	span.End()
}

func finishCitationQualitySpan(span trace.Span, grounded Grounded) {
	if span == nil {
		return
	}
	status := qualityGroundingStatus(grounded.Diagnostics)
	receipt := qualityReceipt{
		component: "citations", outcome: groundingQualityOutcome(status), groundingState: status,
		citationCount: boundedCount(len(grounded.CitedDocumentIDs)), claimCount: boundedCount(len(grounded.Claims)),
		abstained: looksLikeAbstention(grounded.Answer), hasAnswerShape: true,
	}
	span.SetAttributes(qualityAttributes(receipt)...)
	setQualityStatus(span, receipt.outcome, false)
	span.End()
}

// finishMissingQualitySpans makes early returns observable without pretending
// work ran. It emits one bounded not_run sentinel for each absent required
// query stage. This is also the deterministic missing-span failure contract.
func finishMissingQualitySpans(ctx context.Context) {
	fields := qualityFields(ctx)
	for _, name := range fields.tracker.missing() {
		component := qualityComponentForSpan(name)
		receipt := qualityReceipt{component: component, outcome: "not_run"}
		_, span := qualityTracer(ctx).Start(ctx, name, trace.WithAttributes(qualityAttributes(receipt)...))
		span.End()
	}
}

func answerQualityReceipt(result AnswerResult) qualityReceipt {
	outcome := answerQualityOutcome(result)
	receipt := qualityReceipt{
		component: "answer", outcome: outcome,
		provider: qualityProvider(result.Provider), freshness: qualityFreshness(result.RetrievalDiagnostics),
		citationCount: boundedCount(len(result.CitedDocumentIDs)), claimCount: boundedCount(len(result.Claims)),
		abstained: looksLikeAbstention(result.Answer), hasAnswerShape: true,
	}
	if value, ok := qualityEstimatedCostMicroUSD(result.RetrievalDiagnostics); ok {
		receipt.costMicroUSD, receipt.hasCost = value, true
	}
	return receipt
}

func retrievalQualityReceipt(passages []Passage, diag map[string]any, err error) qualityReceipt {
	cacheHit, _ := diag["cache_hit"].(bool)
	return qualityReceipt{
		component: "retrieve", arm: qualityRetrievalRoute(diag), outcome: retrievalQualityOutcome(err, len(passages)),
		outputCount: boundedCount(len(passages)), hasOutput: true, cacheHit: cacheHit, hasCacheHit: true,
		freshness: qualityFreshness(diag),
	}
}

func rerankQualityReceipt(passages []Passage, diag map[string]any) qualityReceipt {
	state := qualityEnumString(diag["rerank"], "skipped", "disabled", "ok")
	outcome := state
	if state == "ok" && boolDiagnostic(diag, "rerank_fallback") {
		outcome = "degraded"
	}
	if state == "unknown" {
		outcome = "error"
	}
	return qualityReceipt{
		component: "rerank", arm: qualityProvider(stringDiagnostic(diag, "rerank_backend")), outcome: outcome,
		outputCount: boundedCount(len(passages)), hasOutput: true,
	}
}

func qualityAttributes(receipt qualityReceipt) []attribute.KeyValue {
	attrs := make([]attribute.KeyValue, 0, qualityMaxAttributes)
	if receipt.component != "" {
		attrs = append(attrs, attribute.String(qualityAttrComponent, qualityComponent(receipt.component)))
	}
	if receipt.arm != "" {
		attrs = append(attrs, attribute.String(qualityAttrArm, qualityArm(receipt.arm)))
	}
	if receipt.mode != "" {
		attrs = append(attrs, attribute.String(qualityAttrMode, qualityMode(receipt.mode)))
	}
	if receipt.outcome != "" {
		attrs = append(attrs, attribute.String(qualityAttrOutcome, qualityOutcome(receipt.outcome)))
	}
	if receipt.provider != "" {
		attrs = append(attrs, attribute.String(qualityAttrProvider, qualityProvider(receipt.provider)))
	}
	if receipt.freshness != "" {
		attrs = append(attrs, attribute.String(qualityAttrFreshness, qualityFreshnessValue(receipt.freshness)))
	}
	if receipt.groundingState != "" {
		attrs = append(attrs, attribute.String(qualityAttrGroundingState, qualityGroundingState(receipt.groundingState)))
	}
	if receipt.hasInput {
		attrs = append(attrs, attribute.Int(qualityAttrInputCount, boundedCount(receipt.inputCount)))
	}
	if receipt.hasOutput {
		attrs = append(attrs, attribute.Int(qualityAttrOutputCount, boundedCount(receipt.outputCount)))
	}
	if receipt.hasCacheHit {
		attrs = append(attrs, attribute.Bool(qualityAttrCacheHit, receipt.cacheHit))
	}
	if receipt.hasCost {
		attrs = append(attrs, attribute.Int64(qualityAttrCostMicroUSD, boundedCostMicroUSD(receipt.costMicroUSD)))
	}
	if receipt.hasAnswerShape {
		attrs = append(attrs,
			attribute.Int(qualityAttrCitationCount, boundedCount(receipt.citationCount)),
			attribute.Int(qualityAttrClaimCount, boundedCount(receipt.claimCount)),
			attribute.Bool(qualityAttrAbstained, receipt.abstained),
		)
	}
	if len(attrs) > qualityMaxAttributes {
		return attrs[:qualityMaxAttributes]
	}
	return attrs
}

func setQualityStatus(span trace.Span, outcome string, failed bool) {
	if failed {
		span.SetStatus(codes.Error, qualityOutcome(outcome))
		return
	}
	if outcome != "not_run" {
		span.SetStatus(codes.Ok, "")
	}
}

func boundedCount(value int) int {
	if value < 0 {
		return 0
	}
	if value > qualityCountMax {
		return qualityCountMax
	}
	return value
}

func boundedCostMicroUSD(value int64) int64 {
	if value < 0 {
		return 0
	}
	if value > qualityCostMicroUSDMax {
		return qualityCostMicroUSDMax
	}
	return value
}

func qualityEstimatedCostMicroUSD(diag map[string]any) (int64, bool) {
	block, ok := diag["llm_cost"].(map[string]any)
	if !ok {
		return 0, false
	}
	value, ok := numberDiagnostic(block["total_cost_usd"])
	if !ok || math.IsNaN(value) || math.IsInf(value, 0) || value < 0 {
		return 0, false
	}
	return boundedCostMicroUSD(int64(math.Round(value * 1_000_000))), true
}

func numberDiagnostic(value any) (float64, bool) {
	switch n := value.(type) {
	case float64:
		return n, true
	case float32:
		return float64(n), true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	default:
		return 0, false
	}
}

func qualityMode(mode string) string {
	return qualityEnum(strings.ToLower(strings.TrimSpace(mode)), "default", "light", "deep", "research", "bench")
}

func qualityComponent(component string) string {
	return qualityEnum(component, "unknown", "answer", "ingest", "retrieve", "rerank", "packing", "synthesis", "citations")
}

func qualityComponentForSpan(name string) string {
	switch name {
	case retrievalQualitySpanName:
		return "retrieve"
	case rerankQualitySpanName:
		return "rerank"
	case packingQualitySpanName:
		return "packing"
	case synthesisQualitySpanName:
		return "synthesis"
	case citationQualitySpanName:
		return "citations"
	default:
		return "unknown"
	}
}

func qualityKnownSpanName(name string) bool {
	return name == answerQualitySpanName || name == ingestQualitySpanName ||
		name == retrievalQualitySpanName || name == rerankQualitySpanName ||
		name == packingQualitySpanName || name == synthesisQualitySpanName ||
		name == citationQualitySpanName
}

func qualityArm(arm string) string {
	return qualityEnum(arm, "unknown",
		"pipeline", "hybrid", "prompt", "grounding", "cross_encoder",
		"openai", "gemini", "openrouter", "groq", "mlx", "cohere", "zeroentropy", "lexical", "extractive", "abstain",
		"batch", "burst", "delta",
		"interactive", "interactive_expand_lite", "interactive_local", "interactive_local_expand_lite", "residual_opt_in",
		"synth", "primary", "primary_info_not_found", "map_reduce_map", "map_reduce_reduce",
		"self_consistency_sample", "corrective_retry", "false_abstention_retry", "completeness_retry",
		"completeness_retry_2", "faithfulness_repair", "query_plan_refine", "hyde_expand")
}

func qualityIngestArm(arm string) string {
	return qualityEnum(strings.ToLower(strings.TrimSpace(arm)), "batch", "batch", "burst", "delta")
}

func qualitySynthesisArm(stage string) string {
	return qualityArm(strings.ToLower(strings.TrimSpace(stage)))
}

func qualityProvider(provider string) string {
	return qualityEnum(strings.ToLower(strings.TrimSpace(provider)), "unknown",
		"openai", "gemini", "openrouter", "groq", "mlx", "cohere", "zeroentropy", "lexical",
		"extractive", "abstain")
}

func qualityOutcome(outcome string) string {
	return qualityEnum(strings.ToLower(strings.TrimSpace(outcome)), "unknown",
		"ok", "partial", "empty", "degraded", "abstained", "no_answer", "denied", "error",
		"timeout", "canceled", "skipped", "disabled", "grounded", "limited", "not_run")
}

func qualityGroundingState(value string) string {
	return qualityEnum(strings.ToLower(strings.TrimSpace(value)), "unknown",
		"ok", "weak", "citations_only", "no_supported_claims", "no_citations", "illegal_citations_stripped")
}

func qualityFreshness(diag map[string]any) string {
	if diag == nil {
		return "unknown"
	}
	for _, key := range []string{"freshness", "freshness_state"} {
		if value := qualityFreshnessValue(stringDiagnostic(diag, key)); value != "unknown" {
			return value
		}
	}
	if boolDiagnostic(diag, "cache_hit") {
		return "cached"
	}
	if boolDiagnostic(diag, "recency_pack") {
		return "recency_adjudicated"
	}
	if _, present := diag["passage_count"]; present {
		return "observed"
	}
	return "unknown"
}

func qualityFreshnessValue(value string) string {
	return qualityEnum(strings.ToLower(strings.TrimSpace(value)), "unknown",
		"current", "degraded", "stale_disclosed", "cached", "observed", "recency_adjudicated", "updated", "unchanged")
}

func qualityRetrievalRoute(diag map[string]any) string {
	return qualityEnum(stringDiagnostic(diag, "retrieve_class"), "hybrid",
		"interactive", "interactive_expand_lite", "interactive_local", "interactive_local_expand_lite", "residual_opt_in")
}

func qualityEnum(value, fallback string, allowed ...string) string {
	for _, candidate := range allowed {
		if value == candidate {
			return candidate
		}
	}
	return fallback
}

func qualityEnumString(value any, allowed ...string) string {
	s, _ := value.(string)
	return qualityEnum(strings.ToLower(strings.TrimSpace(s)), "unknown", allowed...)
}

func stringDiagnostic(diag map[string]any, key string) string {
	value, _ := diag[key].(string)
	return strings.ToLower(strings.TrimSpace(value))
}

func answerQualityOutcome(result AnswerResult) string {
	switch {
	case result.Failure == "denied":
		return "denied"
	case result.Failure != "":
		return "error"
	case boolDiagnostic(result.RetrievalDiagnostics, "degraded_timeout"):
		return "degraded"
	case strings.TrimSpace(result.Answer) == "":
		return "no_answer"
	case looksLikeAbstention(result.Answer):
		return "abstained"
	default:
		return "ok"
	}
}

func retrievalQualityOutcome(err error, count int) string {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.Is(err, context.Canceled):
		return "canceled"
	case err != nil:
		return "error"
	case count == 0:
		return "empty"
	default:
		return "ok"
	}
}

func groundingQualityOutcome(status string) string {
	switch status {
	case "ok", "weak", "citations_only":
		return "grounded"
	case "no_supported_claims", "no_citations", "illegal_citations_stripped":
		return "limited"
	default:
		return "unknown"
	}
}

func qualityGroundingStatus(diag map[string]any) string {
	return qualityGroundingState(stringDiagnostic(diag, "grounding_status"))
}

func boolDiagnostic(diag map[string]any, key string) bool {
	value, _ := diag[key].(bool)
	return value
}
