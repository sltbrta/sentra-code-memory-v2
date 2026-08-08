package hosted

import (
	"context"
	"testing"
	"time"
)

// Issue #282: the optional query-plan LLM refine must be bounded fail-closed
// so it can never consume the retrieval/synthesis deadline reserve. These
// tests never touch the network: provider keys are cleared, and the skip
// paths under test return before any candidate is built.

// clearQueryPlanLLMEnv neutralizes every knob and provider key that could
// make refineQueryPlanLLM attempt a real call or shift budget math.
func clearQueryPlanLLMEnv(t *testing.T) {
	t.Helper()
	unsetBudgetEnv(t)
	for _, k := range []string{
		"OUROBOROS_ERB_QUERY_PLAN_LLM", "OUROBOROS_ERB_QUERY_PLAN_LLM_OFF",
		"OUROBOROS_ERB_QUERY_PLAN_LLM_MS",
		"GEMINI_API_KEY", "GOOGLE_API_KEY", "CEREBRAS_API_KEY",
		"GROQ_API_KEY", "OPENAI_API_KEY",
	} {
		t.Setenv(k, "")
	}
}

func TestQueryPlanLLMBudgetDefaultsAndClamp(t *testing.T) {
	clearQueryPlanLLMEnv(t)
	// No deadline: env default 400ms.
	if d, skip := queryPlanLLMBudget(context.Background()); d != 400*time.Millisecond || skip != "" {
		t.Fatalf("no-deadline budget=%v skip=%q want 400ms, no skip", d, skip)
	}
	// Env clamp: floor 100ms, ceiling 1500ms.
	t.Setenv("OUROBOROS_ERB_QUERY_PLAN_LLM_MS", "5")
	if d, _ := queryPlanLLMBudget(context.Background()); d != 100*time.Millisecond {
		t.Fatalf("floor clamp budget=%v want 100ms", d)
	}
	t.Setenv("OUROBOROS_ERB_QUERY_PLAN_LLM_MS", "60000")
	if d, _ := queryPlanLLMBudget(context.Background()); d != 1500*time.Millisecond {
		t.Fatalf("ceiling clamp budget=%v want 1500ms", d)
	}
}

func TestQueryPlanLLMBudgetClampedToDeadlineReserve(t *testing.T) {
	clearQueryPlanLLMEnv(t)
	t.Setenv("OUROBOROS_ERB_QUERY_PLAN_LLM_MS", "1500")
	t.Setenv("OUROBOROS_ERB_DEADLINE_MARGIN_MS", "2000")
	// 2.5s remaining minus the 2s reserve leaves ~500ms — the 1500ms ask must
	// shrink so retrieval/synthesis keep their margin.
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(2500*time.Millisecond))
	defer cancel()
	d, skip := queryPlanLLMBudget(ctx)
	if skip != "" {
		t.Fatalf("unexpected skip %q with 500ms available", skip)
	}
	if d < 300*time.Millisecond || d > 500*time.Millisecond {
		t.Fatalf("budget=%v want ~500ms (clamped to remaining-deadline reserve)", d)
	}
}

func TestQueryPlanLLMBudgetSkipsInsideReserve(t *testing.T) {
	clearQueryPlanLLMEnv(t)
	t.Setenv("OUROBOROS_ERB_DEADLINE_MARGIN_MS", "2000")
	// Remaining < reserve+floor → the optional call cannot fit: fail closed.
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(2050*time.Millisecond))
	defer cancel()
	d, skip := queryPlanLLMBudget(ctx)
	if skip != "deadline_reserve" || d != 0 {
		t.Fatalf("budget=%v skip=%q want 0, deadline_reserve", d, skip)
	}
	// Already-done context skips regardless of deadline math.
	done, cancel2 := context.WithCancel(context.Background())
	cancel2()
	if _, skip := queryPlanLLMBudget(done); skip != "context_done" {
		t.Fatalf("canceled ctx skip=%q want context_done", skip)
	}
}

func TestRefineQueryPlanLLMSkipsFailClosedNearDeadline(t *testing.T) {
	clearQueryPlanLLMEnv(t)
	t.Setenv("OUROBOROS_ERB_QUERY_PLAN_LLM", "1")
	t.Setenv("OUROBOROS_ERB_DEADLINE_MARGIN_MS", "2000")
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(2050*time.Millisecond))
	defer cancel()

	q := "What is Alpha?"
	in := InferQueryPlan(q)
	in = ApplyServeMode(in, "light")
	if !in.IsThin(q) {
		t.Fatalf("fixture question must be thin, plan=%+v", in)
	}
	start := time.Now()
	out := refineQueryPlanLLM(ctx, q, in)
	if el := time.Since(start); el > 500*time.Millisecond {
		t.Fatalf("fail-closed skip must be immediate, took %v", el)
	}
	if out.LLMSkipped != "deadline_reserve" {
		t.Fatalf("LLMSkipped=%q want deadline_reserve", out.LLMSkipped)
	}
	if out.LLMRefined {
		t.Fatal("skipped refine must not mark plan as LLM-refined")
	}
	// Optional-plan fallback: deterministic plan survives untouched (mode
	// budgets included) apart from the skip stamp.
	in.LLMSkipped = out.LLMSkipped
	if out != in {
		t.Fatalf("skip must preserve deterministic plan: got %+v want %+v", out, in)
	}
}

func TestRefineQueryPlanLLMSkipStampsLedger(t *testing.T) {
	clearQueryPlanLLMEnv(t)
	t.Setenv("OUROBOROS_ERB_QUERY_PLAN_LLM", "1")
	t.Setenv("OUROBOROS_ERB_DEADLINE_MARGIN_MS", "2000")
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(100*time.Millisecond))
	defer cancel()
	led := newLLMLedger(3)
	ctx = withLLMLedger(ctx, led)

	q := "What is Alpha?"
	_ = refineQueryPlanLLM(ctx, q, InferQueryPlan(q))
	diag := map[string]any{}
	led.stampInto(diag)
	b, _ := diag["llm_budget"].(map[string]any)
	skips, _ := b["deadline_skips"].([]map[string]string)
	if len(skips) != 1 || skips[0]["stage"] != "query_plan" || skips[0]["reason"] != "deadline_reserve" {
		t.Fatalf("query_plan skip not stamped in ledger: %#v", b)
	}
	if calls, _ := b["calls"].(int); calls != 0 {
		t.Fatalf("skipped refine must not spend a generation call: %#v", b)
	}
}

// Issue #302 ordering regression: the request-scoped ledger must already be on
// the context when the optional plan refine runs, otherwise the query_plan
// stage records onto a nil ledger and vanishes from the consolidated
// budget/usage diagnostics. planWithLedger owns that ordering.
func TestPlanWithLedgerAttachesLedgerBeforeRefine(t *testing.T) {
	clearQueryPlanLLMEnv(t)
	t.Setenv("OUROBOROS_ERB_QUERY_PLAN_LLM", "1")
	t.Setenv("OUROBOROS_ERB_DEADLINE_MARGIN_MS", "2000")
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(100*time.Millisecond))
	defer cancel()

	q := "What is Alpha?"
	ctx, led, questionType, plan := planWithLedger(ctx, q, "", "light")
	if led == nil {
		t.Fatal("planWithLedger must return a request-scoped ledger")
	}
	if ledgerFrom(ctx) != led {
		t.Fatal("returned context must carry the same ledger the refine recorded onto")
	}
	if plan.LLMSkipped != "deadline_reserve" {
		t.Fatalf("LLMSkipped=%q want deadline_reserve", plan.LLMSkipped)
	}
	if questionType == "" {
		t.Fatal("question type must still resolve when the refine is skipped")
	}

	// The cap is only finalized after the plan settles; the skip recorded before
	// that must survive into the consolidated diagnostics.
	led.setMaxCalls(4)
	diag := map[string]any{}
	led.stampInto(diag)
	b, _ := diag["llm_budget"].(map[string]any)
	if b == nil {
		t.Fatal("llm_budget block missing")
	}
	if got, _ := b["max_calls"].(int); got != 4 {
		t.Fatalf("max_calls=%v want 4 (finalized after planning)", b["max_calls"])
	}
	if got, _ := b["calls"].(int); got != 0 {
		t.Fatalf("planning must not spend a generation call slot: calls=%v", b["calls"])
	}
	skips, _ := b["deadline_skips"].([]map[string]string)
	if len(skips) != 1 || skips[0]["stage"] != "query_plan" || skips[0]["reason"] != "deadline_reserve" {
		t.Fatalf("query_plan skip missing from consolidated diagnostics: %#v", b)
	}
}

// A nil context must not panic the planning path (context.WithTimeout and
// context.WithValue both reject nil parents).
func TestPlanWithLedgerNilContextSafe(t *testing.T) {
	clearQueryPlanLLMEnv(t)
	t.Setenv("OUROBOROS_ERB_QUERY_PLAN_LLM", "1")

	//nolint:staticcheck // deliberately exercising the nil-context guard.
	ctx, led, questionType, plan := planWithLedger(nil, "What is Alpha?", "", "light")
	if ctx == nil {
		t.Fatal("planWithLedger must never return a nil context")
	}
	if led == nil || ledgerFrom(ctx) != led {
		t.Fatal("nil-context path must still attach the request-scoped ledger")
	}
	if questionType == "" {
		t.Fatal("question type must resolve on the nil-context path")
	}
	// Keyless + no deadline: fail open to the deterministic plan, no skip stamp.
	if plan.LLMSkipped != "" || plan.LLMRefined {
		t.Fatalf("keyless nil-context refine must fail open: %+v", plan)
	}
	if ctx.Err() != nil {
		t.Fatalf("substituted context must be live, err=%v", ctx.Err())
	}
}

// Labeled question types stay authoritative through planWithLedger — the refine
// may only fill an inferred type.
func TestPlanWithLedgerKeepsLabeledType(t *testing.T) {
	clearQueryPlanLLMEnv(t)
	_, led, questionType, _ := planWithLedger(context.Background(), "What is Alpha?", "conflicting_info", "light")
	if questionType != "conflicting_info" {
		t.Fatalf("questionType=%q want labeled conflicting_info", questionType)
	}
	if led == nil {
		t.Fatal("ledger must be attached regardless of labeled type")
	}
}

func TestRefineQueryPlanLLMNoDeadlineStaysBoundedAndFailOpen(t *testing.T) {
	clearQueryPlanLLMEnv(t)
	t.Setenv("OUROBOROS_ERB_QUERY_PLAN_LLM", "1")
	// No provider keys: the bounded call path must fail open to the original
	// plan immediately (no candidates → no network), with no skip stamp.
	q := "What is Alpha?"
	in := InferQueryPlan(q)
	start := time.Now()
	out := refineQueryPlanLLM(context.Background(), q, in)
	if el := time.Since(start); el > 500*time.Millisecond {
		t.Fatalf("keyless refine must return immediately, took %v", el)
	}
	if out != in {
		t.Fatalf("keyless refine must fail open to original plan: got %+v want %+v", out, in)
	}
}
