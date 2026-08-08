package hosted

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"
	"time"
)

// unsetBudgetEnv clears every knob that can shift budget math between tests.
func unsetBudgetEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{
		"OUROBOROS_ERB_QUALITY", "OUROBOROS_ERB_BENCHMAX", "OUROBOROS_ERB_BENCH_MAX",
		"OUROBOROS_ERB_MODE", "OUROBOROS_ERB_PROD", "OUROBOROS_ERB_MAX_LLM_CALLS",
		"OUROBOROS_ERB_SELF_CONSISTENCY", "OUROBOROS_ERB_SELF_CONSISTENCY_AUTO",
		"OUROBOROS_ERB_SELF_CONSISTENCY_ALWAYS", "OUROBOROS_ERB_SELF_CONSISTENCY_OFF",
		"OUROBOROS_ERB_MAP_REDUCE", "OUROBOROS_ERB_MAP_REDUCE_FACETS",
		"OUROBOROS_ERB_SYNTH_TIMEOUT_MS", "OUROBOROS_ERB_SYNTH_MAX_PROVIDERS",
		"OUROBOROS_ERB_DEADLINE_MARGIN_MS", "OUROBOROS_ERB_CORRECTIVE",
		"OUROBOROS_ERB_MAX_SYNTH_RETRY", "OUROBOROS_ERB_FAITHFULNESS",
		"OUROBOROS_ERB_FAITHFULNESS_LLM",
		"OUROBOROS_ERB_HEDGE_DELAY_MS", "OUROBOROS_ERB_RATE_LIMIT_COOLDOWN_MS",
	} {
		_ = os.Unsetenv(k)
	}
}

func TestMaxLLMCallsPerMode(t *testing.T) {
	unsetBudgetEnv(t)
	// Prod default: 1 primary + MaxSynthRetry(1), corrective off → 2.
	prod := prodProfileFromEnv()
	if got := maxLLMCallsFor(prod, "What is the refund policy?", "basic", QueryPlan{}); got != 2 {
		t.Fatalf("prod max calls=%d want 2", got)
	}
	// QUALITY with corrective on: 1 + 1 retry + 1 corrective = 3.
	t.Setenv("OUROBOROS_ERB_QUALITY", "1")
	t.Setenv("OUROBOROS_ERB_CORRECTIVE", "1")
	q := prodProfileFromEnv()
	if got := maxLLMCallsFor(q, "What is the refund policy?", "basic", QueryPlan{}); got != 3 {
		t.Fatalf("quality+corrective max calls=%d want 3", got)
	}
	// Optional critic repair is exactly one additional request-scoped call.
	t.Setenv("OUROBOROS_ERB_FAITHFULNESS_LLM", "1")
	if got := maxLLMCallsFor(q, "What is the refund policy?", "basic", QueryPlan{}); got != 4 {
		t.Fatalf("quality+corrective+critic max calls=%d want 4", got)
	}
	// The rollout kill switch removes the critic slot even when the repair knob
	// remains set in the deployment environment.
	t.Setenv("OUROBOROS_ERB_FAITHFULNESS", "0")
	if got := maxLLMCallsFor(q, "What is the refund policy?", "basic", QueryPlan{}); got != 3 {
		t.Fatalf("kill-switched critic reserved a call: got %d want 3", got)
	}
}

func TestMaxLLMCallsBenchmaxBounded(t *testing.T) {
	unsetBudgetEnv(t)
	t.Setenv("OUROBOROS_ERB_BENCHMAX", "1")
	prod := prodProfileFromEnv()
	if !prod.Benchmax {
		t.Fatal("benchmax profile expected")
	}
	// Completeness triggers map-reduce: facets(6)+reduce(1) base + retry(4) + corrective(1).
	got := maxLLMCallsFor(prod, "List every customer and their channel", "completeness", QueryPlan{Completeness: true})
	want := mapReduceMaxFacets() + 1 + prod.MaxSynthRetry + 1
	if got != want {
		t.Fatalf("benchmax map-reduce max calls=%d want %d", got, want)
	}
	// Even benchmax must be explicitly bounded — no unlimited serial chain.
	if got > 32 {
		t.Fatalf("benchmax max calls=%d exceeds sane ceiling 32", got)
	}
	// Basic question: no map-reduce/self-consistency → 1 + 4 + 1 = 6.
	if got := maxLLMCallsFor(prod, "What is the status page URL?", "basic", QueryPlan{}); got != 6 {
		t.Fatalf("benchmax basic max calls=%d want 6", got)
	}
	// Env override wins and stays explicit.
	t.Setenv("OUROBOROS_ERB_MAX_LLM_CALLS", "5")
	if got := maxLLMCallsFor(prod, "List every customer", "completeness", QueryPlan{Completeness: true}); got != 5 {
		t.Fatalf("env override max calls=%d want 5", got)
	}
}

func TestDeadlineMarginOK(t *testing.T) {
	unsetBudgetEnv(t)
	if !deadlineMarginOK(context.Background()) {
		t.Fatal("no deadline should always allow")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if deadlineMarginOK(ctx) {
		t.Fatal("canceled ctx must not allow more calls")
	}
	near, c2 := context.WithDeadline(context.Background(), time.Now().Add(50*time.Millisecond))
	defer c2()
	if deadlineMarginOK(near) {
		t.Fatal("50ms remaining is inside default margin — must skip")
	}
	far, c3 := context.WithDeadline(context.Background(), time.Now().Add(30*time.Second))
	defer c3()
	if !deadlineMarginOK(far) {
		t.Fatal("30s remaining must allow calls")
	}
	// Margin is env-tunable.
	t.Setenv("OUROBOROS_ERB_DEADLINE_MARGIN_MS", "10")
	if !deadlineMarginOK(near) {
		t.Fatal("50ms remaining exceeds 10ms margin — must allow")
	}
}

func TestSynthCallTimeoutEnv(t *testing.T) {
	unsetBudgetEnv(t)
	if got := synthCallTimeout(); got != 90*time.Second {
		t.Fatalf("default synth timeout=%v want 90s (parity with legacy client)", got)
	}
	t.Setenv("OUROBOROS_ERB_SYNTH_TIMEOUT_MS", "5000")
	if got := synthCallTimeout(); got != 5*time.Second {
		t.Fatalf("env synth timeout=%v want 5s", got)
	}
	t.Setenv("OUROBOROS_ERB_SYNTH_TIMEOUT_MS", "50")
	if got := synthCallTimeout(); got != time.Second {
		t.Fatalf("floor synth timeout=%v want 1s", got)
	}
	t.Setenv("OUROBOROS_ERB_SYNTH_TIMEOUT_MS", "99999999")
	if got := synthCallTimeout(); got != 300*time.Second {
		t.Fatalf("cap synth timeout=%v want 300s", got)
	}
}

func TestSynthMaxProvidersAndFacetsBounded(t *testing.T) {
	unsetBudgetEnv(t)
	if got := synthMaxProviders(); got != 4 {
		t.Fatalf("default provider chain cap=%d want 4", got)
	}
	t.Setenv("OUROBOROS_ERB_SYNTH_MAX_PROVIDERS", "2")
	if got := synthMaxProviders(); got != 2 {
		t.Fatalf("env provider cap=%d want 2", got)
	}
	t.Setenv("OUROBOROS_ERB_SYNTH_MAX_PROVIDERS", "99")
	if got := synthMaxProviders(); got > 8 {
		t.Fatalf("provider cap clamp=%d want ≤8", got)
	}
	if got := mapReduceMaxFacets(); got != 6 {
		t.Fatalf("default map-reduce facets=%d want 6", got)
	}
	t.Setenv("OUROBOROS_ERB_MAP_REDUCE_FACETS", "3")
	if got := mapReduceMaxFacets(); got != 3 {
		t.Fatalf("env facets=%d want 3", got)
	}
	t.Setenv("OUROBOROS_ERB_MAP_REDUCE_FACETS", "99")
	if got := mapReduceMaxFacets(); got > 8 {
		t.Fatalf("facets clamp=%d want ≤8", got)
	}
}

func TestLedgerCapEnforcement(t *testing.T) {
	l := newLLMLedger(2)
	l.beginCall("synth", "primary")
	l.beginCall("self_consistency_sample", "sample_2_of_4")
	if l.canSpend(context.Background(), "completeness_retry") {
		t.Fatal("used==max must refuse extra calls")
	}
	diag := map[string]any{}
	l.stampInto(diag)
	b, ok := diag["llm_budget"].(map[string]any)
	if !ok {
		t.Fatalf("llm_budget diag missing: %#v", diag)
	}
	if b["max_calls"] != 2 || b["calls"] != 2 {
		t.Fatalf("budget diag calls mismatch: %#v", b)
	}
	skips, ok := b["deadline_skips"].([]map[string]string)
	if !ok || len(skips) != 1 || skips[0]["reason"] != "call_budget_exceeded" ||
		skips[0]["stage"] != "completeness_retry" {
		t.Fatalf("budget skip not stamped: %#v", b)
	}
	stages, ok := b["stages"].([]map[string]string)
	if !ok || len(stages) != 2 || stages[0]["reason"] != "primary" {
		t.Fatalf("stage reasons not stamped: %#v", b)
	}
}

// Issue #302: planning runs before the per-mode call cap can be computed (the
// cap depends on the settled plan), so the ledger is created with a provisional
// cap and finalized via setMaxCalls. Everything the pre-cap stages recorded —
// skips, provider attempts, token usage — must survive that finalization and
// appear in the consolidated budget/usage diagnostics.
func TestLedgerSetMaxCallsPreservesPrePlanAccounting(t *testing.T) {
	unsetBudgetEnv(t)
	l := newLLMLedger(1) // provisional cap while the plan is still settling
	l.skip("query_plan", "deadline_reserve")
	l.attempt("query_plan", "groq", "llama-3.3-70b-versatile")
	l.recordUsage("query_plan", "groq", "llama-3.3-70b-versatile", 40, 12, 0)
	l.attempt("llm_multiquery", "groq", "llama-3.3-70b-versatile")
	l.recordUsage("llm_multiquery", "groq", "llama-3.3-70b-versatile", 20, 8, 28)

	// Cap finalized once the plan and question type settle.
	l.setMaxCalls(6)
	l.beginCall("synth", "primary")

	diag := map[string]any{}
	l.stampInto(diag)

	b, ok := diag["llm_budget"].(map[string]any)
	if !ok {
		t.Fatalf("llm_budget diag missing: %#v", diag)
	}
	if b["max_calls"] != 6 || b["calls"] != 1 {
		t.Fatalf("finalized cap/calls wrong: %#v", b)
	}
	if b["provider_attempts"] != 2 {
		t.Fatalf("pre-cap provider attempts lost: %#v", b)
	}
	skips, ok := b["deadline_skips"].([]map[string]string)
	if !ok || len(skips) != 1 || skips[0]["stage"] != "query_plan" ||
		skips[0]["reason"] != "deadline_reserve" {
		t.Fatalf("query_plan skip lost across setMaxCalls: %#v", b)
	}

	usage, ok := diag["llm_usage"].(map[string]any)
	if !ok {
		t.Fatalf("llm_usage diag missing: %#v", diag)
	}
	// 40+12 defaulted to a 52 total, plus the explicit 28.
	if usage["input_tokens"] != 60 || usage["output_tokens"] != 20 ||
		usage["total_tokens"] != 80 {
		t.Fatalf("consolidated usage totals wrong: %#v", usage)
	}
	byStage, _ := usage["by_stage"].([]map[string]any)
	stages := map[string]map[string]any{}
	for _, r := range byStage {
		stages[r["stage"].(string)] = r
	}
	qp, ok := stages["query_plan"]
	if !ok {
		t.Fatalf("query_plan stage missing from llm_usage: %#v", byStage)
	}
	if qp["attempts"] != 1 || qp["input_tokens"] != 40 || qp["output_tokens"] != 12 ||
		qp["total_tokens"] != 52 || qp["calls"] != 0 {
		t.Fatalf("query_plan stage usage wrong (planning spends no call slot): %#v", qp)
	}
	if _, ok := stages["llm_multiquery"]; !ok {
		t.Fatalf("retrieval-stage usage missing from llm_usage: %#v", byStage)
	}
	if _, ok := stages["synth"]; !ok {
		t.Fatalf("synth stage missing from llm_usage: %#v", byStage)
	}
}

// setMaxCalls stays bounded: it clamps to a usable floor and never retroactively
// un-budgets a call that was already spent.
func TestLedgerSetMaxCallsBounded(t *testing.T) {
	unsetBudgetEnv(t)
	l := newLLMLedger(4)
	l.setMaxCalls(0)
	if l.max != 1 {
		t.Fatalf("max=%d want floor 1", l.max)
	}
	l.beginCall("synth", "primary")
	l.beginCall("synth_retry", "retry")
	l.setMaxCalls(1)
	if l.max != 2 {
		t.Fatalf("max=%d must never drop below calls already spent (2)", l.max)
	}
	if l.canSpend(context.Background(), "completeness_retry") {
		t.Fatal("used==max must still refuse extra calls after finalization")
	}
	// Nil receiver stays a no-op (ledger-free contexts).
	var nilLedger *llmLedger
	nilLedger.setMaxCalls(3)
}

func TestLedgerRetriesStampedSeparately(t *testing.T) {
	l := newLLMLedger(8)
	l.beginCall("synth", "primary")
	l.beginCall("false_abstention_retry", "abstain_with_relevant_pack")
	l.beginCall("completeness_retry", "thin_completeness")
	diag := map[string]any{}
	l.stampInto(diag)
	b := diag["llm_budget"].(map[string]any)
	if b["calls"] != 3 || b["retries"] != 2 {
		t.Fatalf("calls/retries not separate: %#v", b)
	}
}

// fakeOpenAI points the OpenAI candidate at a test server and clears all other
// provider keys so the chain is exactly one candidate.
func fakeOpenAI(t *testing.T, srv *httptest.Server) {
	t.Helper()
	t.Setenv("OPENAI_API_KEY", "test-key")
	t.Setenv("OUROBOROS_ERB_OPENAI_BASE_URL", srv.URL)
	t.Setenv("GEMINI_API_KEY", "")
	t.Setenv("GOOGLE_API_KEY", "")
	t.Setenv("OPENROUTER_API_KEY", "")
	t.Setenv("GROQ_API_KEY", "")
	t.Setenv("OUROBOROS_BRAIN_LLM", "")
	t.Setenv("OUROBOROS_BRAIN_LLM_SUBSTRATE", "")
}

func TestSynthesizeOnceCanceledContextSkipsCall(t *testing.T) {
	unsetBudgetEnv(t)
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	fakeOpenAI(t, srv)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	l := newLLMLedger(4)
	ctx = withLLMLedger(ctx, l)

	_, _, _, err := synthesizeOnce(ctx, "q", "basic",
		[]Passage{{DocumentID: "d1", Text: "evidence"}}, 400, "", nil, "")
	if err == nil {
		t.Fatal("canceled ctx must surface an error, not a silent fallback call")
	}
	if hits.Load() != 0 {
		t.Fatalf("provider called %d times despite canceled ctx", hits.Load())
	}
	diag := map[string]any{}
	l.stampInto(diag)
	b := diag["llm_budget"].(map[string]any)
	skips, _ := b["deadline_skips"].([]map[string]string)
	if len(skips) != 1 || skips[0]["reason"] != "context_canceled" {
		t.Fatalf("expected context_canceled skip, got %#v", b)
	}
	if b["provider_attempts"] != 0 {
		t.Fatalf("no provider attempt should be recorded: %#v", b)
	}
}

func TestSynthesizeOnceTimeoutClampedToDeadline(t *testing.T) {
	unsetBudgetEnv(t)
	// Margin small so the attempt proceeds; deadline far below synth timeout.
	t.Setenv("OUROBOROS_ERB_DEADLINE_MARGIN_MS", "100")
	var hits atomic.Int32
	// Raw listener that accepts and never responds — no server teardown wait.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	done := make(chan struct{})
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			hits.Add(1)
			go func() {
				<-done
				_ = conn.Close()
			}()
		}
	}()
	defer close(done)
	defer func() { _ = ln.Close() }()
	t.Setenv("OPENAI_API_KEY", "test-key")
	t.Setenv("OUROBOROS_ERB_OPENAI_BASE_URL", "http://"+ln.Addr().String())
	t.Setenv("GEMINI_API_KEY", "")
	t.Setenv("GOOGLE_API_KEY", "")
	t.Setenv("OPENROUTER_API_KEY", "")
	t.Setenv("GROQ_API_KEY", "")
	t.Setenv("OUROBOROS_BRAIN_LLM", "")
	t.Setenv("OUROBOROS_BRAIN_LLM_SUBSTRATE", "")

	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(1200*time.Millisecond))
	defer cancel()
	l := newLLMLedger(4)
	ctx = withLLMLedger(ctx, l)

	start := time.Now()
	_, _, _, err = synthesizeOnce(ctx, "q", "basic",
		[]Passage{{DocumentID: "d1", Text: "evidence"}}, 400, "", nil, "")
	el := time.Since(start)
	if err == nil {
		t.Fatal("hanging provider must error out at the request deadline")
	}
	// 90s default synth timeout must be clamped to the ~1.2s remaining deadline:
	// a retry/call can never extend the overall request deadline.
	if el > 4*time.Second {
		t.Fatalf("synth call ran %v — deadline not enforced (want ~1.2s)", el)
	}
	if hits.Load() != 1 {
		t.Fatalf("want exactly 1 clamped attempt, got %d", hits.Load())
	}
}

func TestSynthesizeOnceRecordsTokensAndCalls(t *testing.T) {
	unsetBudgetEnv(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{
				"message": map[string]string{
					"content": `{"answer":"The limit is 10.","cited_document_ids":["d1"]}`,
				},
			}},
			"usage": map[string]int{
				"prompt_tokens":     11,
				"completion_tokens": 7,
				"total_tokens":      18,
			},
		})
	}))
	defer srv.Close()
	fakeOpenAI(t, srv)

	l := newLLMLedger(4)
	ctx := withLLMLedger(context.Background(), l)
	l.beginCall("synth", "primary")
	raw, provider, _, err := synthesizeOnce(ctx, "q", "basic",
		[]Passage{{DocumentID: "d1", Text: "The limit is 10."}}, 400, "", nil, "")
	if err != nil {
		t.Fatalf("synth failed: %v", err)
	}
	if provider != "openai" || raw.Answer == "" {
		t.Fatalf("unexpected provider/answer: %q %q", provider, raw.Answer)
	}
	diag := map[string]any{}
	l.stampInto(diag)
	toks, ok := diag["llm_tokens"].(map[string]int)
	if !ok || toks["prompt"] != 11 || toks["completion"] != 7 || toks["total"] != 18 {
		t.Fatalf("token metrics not stamped separately: %#v", diag)
	}
	b := diag["llm_budget"].(map[string]any)
	if b["provider_attempts"] != 1 || b["calls"] != 1 {
		t.Fatalf("call/attempt counts wrong: %#v", b)
	}
}

func TestMapReduceSkipsMapStageOnDeadline(t *testing.T) {
	unsetBudgetEnv(t)
	t.Setenv("OUROBOROS_ERB_QUALITY", "1")
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	fakeOpenAI(t, srv)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	l := newLLMLedger(12)
	ctx = withLLMLedger(ctx, l)

	c := &Client{}
	passages := []Passage{
		{DocumentID: "d1", Text: "Project Falcon owner is Ada. Status: green."},
		{DocumentID: "d2", Text: "Project Falcon TTM target is Q3."},
	}
	_, _, _, err := c.mapReduceSynthesize(ctx, "Who owns Project Falcon and what is the TTM?",
		"project_related", passages, 400, nil, "")
	if err == nil {
		t.Fatal("canceled ctx must surface error")
	}
	if hits.Load() != 0 {
		t.Fatalf("map stage fanned out %d provider calls despite canceled ctx", hits.Load())
	}
	diag := map[string]any{}
	l.stampInto(diag)
	b := diag["llm_budget"].(map[string]any)
	skips, _ := b["deadline_skips"].([]map[string]string)
	found := false
	for _, s := range skips {
		if s["stage"] == "map_reduce" {
			found = true
		}
	}
	if !found {
		t.Fatalf("map_reduce deadline skip not stamped: %#v", b)
	}
}

func TestCanSpendRecordsDeadlineSkipOnce(t *testing.T) {
	l := newLLMLedger(8)
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(20*time.Millisecond))
	defer cancel()
	if l.canSpend(ctx, "completeness_retry") {
		t.Fatal("near deadline must refuse")
	}
	if l.canSpend(ctx, "completeness_retry_2") {
		t.Fatal("near deadline must refuse again")
	}
	diag := map[string]any{}
	l.stampInto(diag)
	b := diag["llm_budget"].(map[string]any)
	skips, _ := b["deadline_skips"].([]map[string]string)
	if len(skips) != 2 || skips[0]["reason"] != "deadline_margin" {
		t.Fatalf("deadline skips not stamped per stage: %#v", b)
	}
}
