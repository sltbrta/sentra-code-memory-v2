package hosted

import (
	"context"
	"sync"
	"time"
)

// Nested retrieval budgets (issue #283) complement issue #278's generation
// ledger. One request-scoped ledger covers exhaustive maps, agentic
// reformulation/gap retrieval, and corrective retrieval. Reservations are
// atomic so parallel fan-out cannot overspend the call cap.
//
// The diagnostics contain counters and stage names only: never queries,
// passages, filter values, gold IDs, ACL material, or citations.
const (
	defaultRetrievalExpansionCalls   = 8
	defaultRetrievalExpansionDepth   = 1
	defaultRetrievalExpansionTimeout = 15 * time.Second
	maxRetrievalExpansionCalls       = 16
	maxRetrievalExpansionDepth       = 2
	maxRetrievalExpansionTimeout     = 60 * time.Second
	maxRetrievalExpansionEvents      = 16
)

type retrievalExpansionLedger struct {
	mu               sync.Mutex
	maxCalls         int
	maxDepth         int
	timeout          time.Duration
	deadline         time.Time
	calls            int
	maxObservedDepth int
	stages           []map[string]any
	skips            []map[string]string
}

func newRetrievalExpansionLedger(maxCalls, maxDepth int, timeout time.Duration) *retrievalExpansionLedger {
	if maxCalls < 1 {
		maxCalls = 1
	}
	if maxCalls > maxRetrievalExpansionCalls {
		maxCalls = maxRetrievalExpansionCalls
	}
	if maxDepth < 1 {
		maxDepth = 1
	}
	if maxDepth > maxRetrievalExpansionDepth {
		maxDepth = maxRetrievalExpansionDepth
	}
	if timeout <= 0 {
		timeout = defaultRetrievalExpansionTimeout
	}
	if timeout > maxRetrievalExpansionTimeout {
		timeout = maxRetrievalExpansionTimeout
	}
	now := time.Now()
	return &retrievalExpansionLedger{
		maxCalls: maxCalls,
		maxDepth: maxDepth,
		timeout:  timeout,
		deadline: now.Add(timeout),
	}
}

func newRequestRetrievalExpansionLedger() *retrievalExpansionLedger {
	maxCalls := envInt("OUROBOROS_ERB_MAX_EXPANSION_CALLS", defaultRetrievalExpansionCalls)
	maxDepth := envInt("OUROBOROS_ERB_MAX_EXPANSION_DEPTH", defaultRetrievalExpansionDepth)
	timeoutMS := envInt("OUROBOROS_ERB_EXPANSION_TIMEOUT_MS", int(defaultRetrievalExpansionTimeout/time.Millisecond))
	return newRetrievalExpansionLedger(maxCalls, maxDepth, time.Duration(timeoutMS)*time.Millisecond)
}

type retrievalExpansionLedgerCtxKey struct{}
type retrievalExpansionDepthCtxKey struct{}

func withRetrievalExpansionLedger(ctx context.Context, ledger *retrievalExpansionLedger) context.Context {
	if ledger == nil {
		return ctx
	}
	return context.WithValue(ctx, retrievalExpansionLedgerCtxKey{}, ledger)
}

func retrievalExpansionLedgerFrom(ctx context.Context) *retrievalExpansionLedger {
	if ctx == nil {
		return nil
	}
	ledger, _ := ctx.Value(retrievalExpansionLedgerCtxKey{}).(*retrievalExpansionLedger)
	return ledger
}

func retrievalExpansionDepthFrom(ctx context.Context) int {
	if ctx == nil {
		return 0
	}
	depth, _ := ctx.Value(retrievalExpansionDepthCtxKey{}).(int)
	return depth
}

// budgetContext applies the ledger's absolute expansion deadline to all work
// in an expansion stage, including facet discovery and sibling hydration.
func (l *retrievalExpansionLedger) budgetContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if l == nil {
		return ctx, func() {}
	}
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithDeadline(ctx, l.deadline)
}

// reserve atomically admits one nested retrieve and returns a depth-tagged
// context clamped to both the caller and the request's expansion deadline.
func (l *retrievalExpansionLedger) reserve(ctx context.Context, stage string) (context.Context, context.CancelFunc, bool) {
	if l == nil {
		return ctx, func() {}, true
	}
	if ctx == nil {
		ctx = context.Background()
	}
	depth := retrievalExpansionDepthFrom(ctx) + 1

	l.mu.Lock()
	reason := ""
	switch {
	case !time.Now().Before(l.deadline):
		reason = "time_budget_exceeded"
	case ctx.Err() != nil:
		reason = "context_canceled"
	case depth > l.maxDepth:
		reason = "depth_budget_exceeded"
	case l.calls >= l.maxCalls:
		reason = "call_budget_exceeded"
	}
	if reason != "" {
		l.skipLocked(stage, reason)
		l.mu.Unlock()
		return ctx, func() {}, false
	}
	l.calls++
	if depth > l.maxObservedDepth {
		l.maxObservedDepth = depth
	}
	if len(l.stages) < maxRetrievalExpansionEvents {
		l.stages = append(l.stages, map[string]any{"stage": stage, "depth": depth})
	}
	l.mu.Unlock()

	callCtx := context.WithValue(ctx, retrievalExpansionDepthCtxKey{}, depth)
	callCtx, cancel := context.WithDeadline(callCtx, l.deadline)
	return callCtx, cancel, true
}

// canContinue prevents an oversized configured round count from spinning after
// the shared call/time envelope is exhausted. It does not spend a call.
func (l *retrievalExpansionLedger) canContinue(ctx context.Context, stage string) bool {
	if l == nil {
		return ctx == nil || ctx.Err() == nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	switch {
	case !time.Now().Before(l.deadline):
		l.skipLocked(stage, "time_budget_exceeded")
		return false
	case ctx != nil && ctx.Err() != nil:
		l.skipLocked(stage, "context_canceled")
		return false
	case l.calls >= l.maxCalls:
		l.skipLocked(stage, "call_budget_exceeded")
		return false
	default:
		return true
	}
}

func (l *retrievalExpansionLedger) skipLocked(stage, reason string) {
	for _, skip := range l.skips {
		if skip["stage"] == stage && skip["reason"] == reason {
			return
		}
	}
	if len(l.skips) < maxRetrievalExpansionEvents {
		l.skips = append(l.skips, map[string]string{"stage": stage, "reason": reason})
	}
}

func (l *retrievalExpansionLedger) stampInto(diag map[string]any) {
	if l == nil || diag == nil {
		return
	}
	l.mu.Lock()
	budget := map[string]any{
		"scope":              "nested_expansion",
		"max_calls":          l.maxCalls,
		"calls":              l.calls,
		"max_depth":          l.maxDepth,
		"max_observed_depth": l.maxObservedDepth,
		"timeout_ms":         l.timeout.Milliseconds(),
		"stages":             append([]map[string]any(nil), l.stages...),
	}
	if len(l.skips) > 0 {
		budget["skips"] = append([]map[string]string(nil), l.skips...)
	}
	l.mu.Unlock()
	diag["retrieval_budget"] = budget
}

func stampRetrievalExpansionBudget(diag map[string]any, ctx context.Context) {
	retrievalExpansionLedgerFrom(ctx).stampInto(diag)
}

// expansionRetrieve is the only answer-time entry to a nested RetrieveOpts.
// It preserves the caller-authorized scope, never accepts gold IDs, always
// uses ExpandLite, and atomically reserves depth/call/time budget first.
func (c *Client) expansionRetrieve(
	ctx context.Context,
	stage, question string,
	topK int,
	questionType string,
	sourceTypes []string,
	filter map[string]any,
) ([]Passage, map[string]any, error, bool) {
	ledger := retrievalExpansionLedgerFrom(ctx)
	callCtx, cancel, ok := ledger.reserve(ctx, stage)
	if !ok {
		return nil, nil, nil, false
	}
	defer cancel()
	passages, diag, err := c.RetrieveOpts(callCtx, question, expansionRetrieveOptions(
		topK, questionType, sourceTypes, filter,
	))
	return passages, diag, err, true
}

func expansionRetrieveOptions(
	topK int,
	questionType string,
	sourceTypes []string,
	filter map[string]any,
) RetrieveOptions {
	return RetrieveOptions{
		TopK:         topK,
		QuestionType: questionType,
		SourceTypes:  append([]string(nil), sourceTypes...),
		Filter:       filter,
		ExpandLite:   true,
		// GoldDocIDs intentionally omitted: production expansion must not use
		// offline answer labels to shape retrieval, caching, or citations.
	}
}
