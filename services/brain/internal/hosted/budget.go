package hosted

import (
	"context"
	"sort"
	"strings"
	"sync"
	"time"
)

// Generation-call budgets (issue #278). Every LLM synthesis call gets an
// explicit timeout clamped to the request's remaining deadline, extra calls
// (retries, repairs, map-reduce, self-consistency) are capped per mode, and
// every skip is stamped diagnostically. No retry ever creates a fresh context,
// so the overall request deadline can only shrink a call — never extend it.
//
// Env knobs (ProdProfile conventions):
//
//	OUROBOROS_ERB_MAX_LLM_CALLS         hard cap on generation calls per request
//	OUROBOROS_ERB_SYNTH_TIMEOUT_MS      per-attempt timeout (default 90s, 1s–300s)
//	OUROBOROS_ERB_SYNTH_MAX_PROVIDERS   provider fallback chain cap (default 4)
//	OUROBOROS_ERB_DEADLINE_MARGIN_MS    skip extra calls when less remains (default 2s)
//	OUROBOROS_ERB_MAP_REDUCE_FACETS     map stage fan-out cap (default 6, max 8)
//	OUROBOROS_ERB_FAITHFULNESS           final critic kill switch (default on; 0 rolls back)
//	OUROBOROS_ERB_FAITHFULNESS_LLM       allow one final critic repair call (default off)
//
// Usage/cost accounting (issue #302): the ledger also attributes token usage,
// attempts, and missing-usage counts per pipeline stage and per
// provider/model, and stamps an llm_usage block plus an llm_cost block priced
// by the configured table (see cost.go). Counters only — never prompts,
// evidence, or gold — so the diagnostics stay sanitized-safe.

// llmLedger bounds and audits generation calls for one request. Safe for the
// parallel map-reduce fan-out.
type llmLedger struct {
	mu        sync.Mutex
	max       int
	used      int
	retries   int
	attempts  int
	stages    []map[string]string
	skips     []map[string]string
	providers []map[string]any
	tokPrompt int
	tokCompl  int
	tokTotal  int
	// Usage/cost accounting (issue #302): per pipeline stage and per
	// provider/model. Keys are bounded; extra keys are dropped, never grown.
	byStage      map[string]*usageBucket
	byModel      map[string]*usageBucket
	missingUsage int
}

// usageBucket accumulates call/attempt/token counters for one attribution key
// (a pipeline stage name or a "provider/model" pair).
type usageBucket struct {
	calls    int
	retries  int
	attempts int
	inTok    int
	outTok   int
	totalTok int
	missing  int
}

const (
	maxLedgerStages    = 32
	maxLedgerSkips     = 16
	maxProviderEvents  = 16
	maxLedgerUsageKeys = 24
)

func newLLMLedger(max int) *llmLedger {
	if max < 1 {
		max = 1
	}
	return &llmLedger{
		max:     max,
		byStage: map[string]*usageBucket{},
		byModel: map[string]*usageBucket{},
	}
}

type llmLedgerCtxKey struct{}

type llmStageCtxKey struct{}

// withLLMStage tags the request context with the pipeline stage that owns the
// next generation call, so usage recorded inside synthesizeOnce is attributed
// correctly even under parallel fan-out (map-reduce facets).
func withLLMStage(ctx context.Context, stage string) context.Context {
	if ctx == nil || stage == "" {
		return ctx
	}
	return context.WithValue(ctx, llmStageCtxKey{}, stage)
}

// llmStageFrom reports the current pipeline stage (default "synth").
func llmStageFrom(ctx context.Context) string {
	if ctx == nil {
		return "synth"
	}
	if s, ok := ctx.Value(llmStageCtxKey{}).(string); ok && s != "" {
		return s
	}
	return "synth"
}

func withLLMLedger(ctx context.Context, l *llmLedger) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if l == nil {
		return ctx
	}
	return context.WithValue(ctx, llmLedgerCtxKey{}, l)
}

// setMaxCalls finalizes this request's generation-call cap (issue #278) after
// the query plan and question type settle. The ledger is attached to the
// request context earlier — before the optional query-plan refine (issue #302)
// — so planning and retrieval-stage helpers record their skips, attempts, and
// token usage into the same consolidated diagnostics. Those stages record
// counters only and never spend a call slot, so the cap can be set afterwards
// without losing accounting. Defensive: the cap never drops below what has
// already been spent, so an already-spent call can never be retroactively
// un-budgeted.
func (l *llmLedger) setMaxCalls(max int) {
	if l == nil {
		return
	}
	if max < 1 {
		max = 1
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if max < l.used {
		max = l.used
	}
	l.max = max
}

func ledgerFrom(ctx context.Context) *llmLedger {
	if ctx == nil {
		return nil
	}
	l, _ := ctx.Value(llmLedgerCtxKey{}).(*llmLedger)
	return l
}

// beginCall records one generation call with the reason it exists. Calls whose
// stage names a retry/repair count toward the retries diagnostic.
func (l *llmLedger) beginCall(stage, reason string) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.used++
	isRetry := strings.Contains(stage, "retry")
	if isRetry {
		l.retries++
	}
	if b := usageBucketFor(l.byStage, stage); b != nil {
		b.calls++
		if isRetry {
			b.retries++
		}
	}
	if len(l.stages) < maxLedgerStages {
		l.stages = append(l.stages, map[string]string{"stage": stage, "reason": reason})
	}
}

// attempt records one raw provider HTTP attempt inside a generation call,
// attributed to the pipeline stage and the provider/model targeted.
func (l *llmLedger) attempt(stage, provider, model string) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.attempts++
	if b := usageBucketFor(l.byModel, providerModelKey(provider, model)); b != nil {
		b.attempts++
	}
	if b := usageBucketFor(l.byStage, stage); b != nil {
		b.attempts++
	}
}

// recordUsage stores the provider-reported token counts for one successful
// generation call, attributed to stage and provider/model. Responses without
// any usage fields are counted as missing usage — never guessed — so cost
// summaries can report coverage honestly.
func (l *llmLedger) recordUsage(stage, provider, model string, prompt, completion, total int) {
	if l == nil {
		return
	}
	if prompt == 0 && completion == 0 && total == 0 {
		l.mu.Lock()
		defer l.mu.Unlock()
		l.missingUsage++
		if b := usageBucketFor(l.byModel, providerModelKey(provider, model)); b != nil {
			b.missing++
		}
		if b := usageBucketFor(l.byStage, stage); b != nil {
			b.missing++
		}
		return
	}
	if total == 0 {
		total = prompt + completion
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.tokPrompt += prompt
	l.tokCompl += completion
	l.tokTotal += total
	for _, b := range []*usageBucket{
		usageBucketFor(l.byModel, providerModelKey(provider, model)),
		usageBucketFor(l.byStage, stage),
	} {
		if b == nil {
			continue
		}
		b.inTok += prompt
		b.outTok += completion
		b.totalTok += total
	}
}

// usageBucketFor returns the bucket for key, creating it within the bounded
// key cap. Buckets are created lazily so the maps stay nil-free only when used.
func usageBucketFor(m map[string]*usageBucket, key string) *usageBucket {
	if key == "" {
		key = "unknown"
	}
	if b, ok := m[key]; ok {
		return b
	}
	if len(m) >= maxLedgerUsageKeys {
		return nil
	}
	b := &usageBucket{}
	m[key] = b
	return b
}

func (l *llmLedger) skip(stage, reason string) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.skips) < maxLedgerSkips {
		l.skips = append(l.skips, map[string]string{"stage": stage, "reason": reason})
	}
}

// providerEvent records bounded, sanitized provider-health and hedge events.
// It never includes prompts, response bodies, credentials, or evidence.
func (l *llmLedger) providerEvent(provider, event string, wait time.Duration) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.providers) >= maxProviderEvents {
		return
	}
	row := map[string]any{"provider": provider, "event": event}
	if wait > 0 {
		row["wait_ms"] = wait.Milliseconds()
	}
	l.providers = append(l.providers, row)
}

// canSpend reports whether one more generation call fits the request: the
// per-mode call cap must not be exceeded and the remaining deadline must leave
// margin for a real attempt. Refusals are stamped once per stage.
func (l *llmLedger) canSpend(ctx context.Context, stage string) bool {
	if l == nil {
		return true
	}
	l.mu.Lock()
	full := l.used >= l.max
	l.mu.Unlock()
	if full {
		l.skip(stage, "call_budget_exceeded")
		return false
	}
	if !deadlineMarginOK(ctx) {
		if ctx != nil && ctx.Err() != nil {
			l.skip(stage, "context_canceled")
		} else {
			l.skip(stage, "deadline_margin")
		}
		return false
	}
	return true
}

// stampInto exposes bounded diagnostics: calls, retries, provider attempts,
// per-stage reasons, deadline/budget skips — separate from latency, grounding,
// citation, and token keys. It also stamps issue #302's sanitized usage/cost
// blocks (llm_usage / llm_cost). All state is snapshotted under one lock;
// stamping itself happens unlocked so cost lookups can never deadlock.
func (l *llmLedger) stampInto(diag map[string]any) {
	if l == nil || diag == nil {
		return
	}
	l.mu.Lock()
	cloneStrings := func(src []map[string]string) []map[string]string {
		out := make([]map[string]string, len(src))
		for i, row := range src {
			out[i] = make(map[string]string, len(row))
			for key, value := range row {
				out[i][key] = value
			}
		}
		return out
	}
	cloneAny := func(src []map[string]any) []map[string]any {
		out := make([]map[string]any, len(src))
		for i, row := range src {
			out[i] = make(map[string]any, len(row))
			for key, value := range row {
				out[i][key] = value
			}
		}
		return out
	}
	budget := map[string]any{
		"max_calls":         l.max,
		"calls":             l.used,
		"retries":           l.retries,
		"provider_attempts": l.attempts,
		"stages":            cloneStrings(l.stages),
		"synth_timeout_ms":  synthCallTimeout().Milliseconds(),
	}
	if len(l.skips) > 0 {
		budget["deadline_skips"] = cloneStrings(l.skips)
	}
	if len(l.providers) > 0 {
		budget["provider_health"] = cloneAny(l.providers)
	}
	var tokens map[string]int
	if l.tokTotal > 0 || l.tokPrompt > 0 || l.tokCompl > 0 {
		tokens = map[string]int{
			"prompt":     l.tokPrompt,
			"completion": l.tokCompl,
			"total":      l.tokTotal,
		}
	}
	snap := l.usageSnapshotLocked()
	l.mu.Unlock()

	diag["llm_budget"] = budget
	if tokens != nil {
		diag["llm_tokens"] = tokens
	}
	stampLLMUsage(diag, snap)
	stampLLMCost(diag, snap.models, snap.missing)
}

// usageRow is one flattened attribution bucket for diagnostic stamping.
type usageRow struct {
	key      string
	calls    int
	retries  int
	attempts int
	inTok    int
	outTok   int
	totalTok int
	missing  int
}

// usageSnapshot is a consistent copy of the ledger's usage counters.
type usageSnapshot struct {
	calls    int
	retries  int
	attempts int
	inTok    int
	outTok   int
	totalTok int
	missing  int
	models   []usageRow
	stages   []usageRow
}

// usageSnapshotLocked copies usage counters; caller must hold l.mu.
func (l *llmLedger) usageSnapshotLocked() usageSnapshot {
	rows := func(m map[string]*usageBucket, requireActivity bool) []usageRow {
		out := make([]usageRow, 0, len(m))
		for k, b := range m {
			if requireActivity && b.attempts == 0 && b.inTok == 0 &&
				b.outTok == 0 && b.missing == 0 {
				continue
			}
			out = append(out, usageRow{
				key: k, calls: b.calls, retries: b.retries, attempts: b.attempts,
				inTok: b.inTok, outTok: b.outTok, totalTok: b.totalTok, missing: b.missing,
			})
		}
		sort.Slice(out, func(i, j int) bool { return out[i].key < out[j].key })
		return out
	}
	return usageSnapshot{
		calls:    l.used,
		retries:  l.retries,
		attempts: l.attempts,
		inTok:    l.tokPrompt,
		outTok:   l.tokCompl,
		totalTok: l.tokTotal,
		missing:  l.missingUsage,
		models:   rows(l.byModel, true),
		stages:   rows(l.byStage, false),
	}
}

// modelUsageSnapshot returns a stable (sorted) copy of per-provider/model
// usage for tests and cost stamping. Only buckets with real activity count.
func (l *llmLedger) modelUsageSnapshot() []usageRow {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.usageSnapshotLocked().models
}

// stampLLMUsage writes the sanitized-safe llm_usage block: token counts,
// attempts, retries, and missing-usage counts attributed per provider/model
// and per pipeline stage. Counters only — never prompts or evidence.
func stampLLMUsage(diag map[string]any, snap usageSnapshot) {
	if snap.attempts == 0 && snap.totalTok == 0 && snap.inTok == 0 &&
		snap.outTok == 0 && snap.missing == 0 {
		return
	}
	rowList := func(rows []usageRow, keyName string) []map[string]any {
		out := make([]map[string]any, 0, len(rows))
		for _, r := range rows {
			out = append(out, map[string]any{
				keyName:         r.key,
				"calls":         r.calls,
				"retries":       r.retries,
				"attempts":      r.attempts,
				"input_tokens":  r.inTok,
				"output_tokens": r.outTok,
				"total_tokens":  r.totalTok,
				"missing_usage": r.missing,
			})
		}
		return out
	}
	diag["llm_usage"] = map[string]any{
		"calls":               snap.calls,
		"retries":             snap.retries,
		"attempts":            snap.attempts,
		"input_tokens":        snap.inTok,
		"output_tokens":       snap.outTok,
		"total_tokens":        snap.totalTok,
		"missing_usage_calls": snap.missing,
		"by_provider_model":   rowList(snap.models, "provider_model"),
		"by_stage":            rowList(snap.stages, "stage"),
	}
}

// stampLLMBudget writes generation diagnostics and the companion nested
// retrieval-expansion diagnostics for this request's answer path.
func stampLLMBudget(diag map[string]any, ctx context.Context) {
	ledgerFrom(ctx).stampInto(diag)
	stampRetrievalExpansionBudget(diag, ctx)
}

// deadlineMargin is the minimum remaining time required to start another
// generation call. Override: OUROBOROS_ERB_DEADLINE_MARGIN_MS.
func deadlineMargin() time.Duration {
	ms := envInt("OUROBOROS_ERB_DEADLINE_MARGIN_MS", 2000)
	if ms < 0 {
		ms = 0
	}
	return time.Duration(ms) * time.Millisecond
}

// deadlineMarginOK reports whether ctx allows another generation call: not
// canceled, and (when a deadline exists) more than the margin remains.
func deadlineMarginOK(ctx context.Context) bool {
	if ctx == nil {
		return true
	}
	if ctx.Err() != nil {
		return false
	}
	dl, ok := ctx.Deadline()
	if !ok {
		return true
	}
	return time.Until(dl) > deadlineMargin()
}

// synthCallTimeout is the explicit per-attempt generation timeout. It is
// always clamped to the request's remaining deadline at call time, so it can
// never extend the overall deadline. Override: OUROBOROS_ERB_SYNTH_TIMEOUT_MS.
func synthCallTimeout() time.Duration {
	ms := envInt("OUROBOROS_ERB_SYNTH_TIMEOUT_MS", 90000)
	if ms < 1000 {
		ms = 1000
	}
	if ms > 300000 {
		ms = 300000
	}
	return time.Duration(ms) * time.Millisecond
}

// synthMaxProviders caps the serial provider fallback chain per generation
// call (4 = full current chain; benchmax included).
func synthMaxProviders() int {
	n := envInt("OUROBOROS_ERB_SYNTH_MAX_PROVIDERS", 4)
	if n < 1 {
		n = 1
	}
	if n > 8 {
		n = 8
	}
	return n
}

// mapReduceMaxFacets caps the map stage fan-out (plus one reduce call).
func mapReduceMaxFacets() int {
	n := envInt("OUROBOROS_ERB_MAP_REDUCE_FACETS", 6)
	if n < 1 {
		n = 1
	}
	if n > 8 {
		n = 8
	}
	return n
}

// maxLLMCallsFor is the explicit per-mode generation-call budget: one primary
// synthesis (or self-consistency samples, or map-reduce facets+reduce), plus
// synth retries and the corrective re-synth when those modes are active.
// Override: OUROBOROS_ERB_MAX_LLM_CALLS.
func maxLLMCallsFor(prod ProdProfile, question, questionType string, plan QueryPlan) int {
	if v := envInt("OUROBOROS_ERB_MAX_LLM_CALLS", 0); v > 0 {
		return v
	}
	base := 1
	if wantsMapReduceSynth(questionType, plan) {
		base = mapReduceMaxFacets() + 1
	} else if sc := selfConsistencyWanted(question); sc > base {
		base = sc
	}
	max := base + prod.MaxSynthRetry
	if prod.Corrective {
		max++
	}
	if faithfulnessLLMEnabled() {
		max++
	}
	if max < 1 {
		max = 1
	}
	return max
}
