package hosted

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestRetrievalExpansionLedgerBoundsCallsAndDepth(t *testing.T) {
	ledger := newRetrievalExpansionLedger(2, 1, time.Second)
	ctx := withRetrievalExpansionLedger(context.Background(), ledger)

	first, cancelFirst, ok := ledger.reserve(ctx, "agentic_reformulate")
	if !ok {
		t.Fatal("first bounded expansion call must remain available")
	}
	defer cancelFirst()
	if depth := retrievalExpansionDepthFrom(first); depth != 1 {
		t.Fatalf("first expansion depth = %d, want 1", depth)
	}
	if _, _, ok := ledger.reserve(first, "recursive_retrieve"); ok {
		t.Fatal("depth-2 nested retrieve must be refused by the default depth-1 ledger")
	}

	_, cancelSecond, ok := ledger.reserve(ctx, "agentic_gap")
	if !ok {
		t.Fatal("second root expansion call must fit call budget")
	}
	cancelSecond()
	if _, _, ok := ledger.reserve(ctx, "corrective_retrieve"); ok {
		t.Fatal("third expansion call must be refused at max_calls=2")
	}

	diag := map[string]any{}
	ledger.stampInto(diag)
	budget, ok := diag["retrieval_budget"].(map[string]any)
	if !ok {
		t.Fatalf("retrieval_budget missing: %#v", diag)
	}
	if budget["calls"] != 2 || budget["max_calls"] != 2 ||
		budget["max_depth"] != 1 || budget["max_observed_depth"] != 1 {
		t.Fatalf("unexpected retrieval budget counters: %#v", budget)
	}
	skips, ok := budget["skips"].([]map[string]string)
	if !ok || !hasExpansionSkip(skips, "recursive_retrieve", "depth_budget_exceeded") ||
		!hasExpansionSkip(skips, "corrective_retrieve", "call_budget_exceeded") {
		t.Fatalf("expected depth and call skips, got %#v", budget["skips"])
	}
	encoded, err := json.Marshal(budget)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"query", "passage", "gold", "citation", "principal", "filter"} {
		if strings.Contains(strings.ToLower(string(encoded)), forbidden) {
			t.Fatalf("retrieval budget diagnostics leaked forbidden field %q: %s", forbidden, encoded)
		}
	}
}

func TestRetrievalExpansionLedgerHonorsCancellationAndTime(t *testing.T) {
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	ledger := newRetrievalExpansionLedger(2, 1, time.Second)
	if _, _, ok := ledger.reserve(canceled, "agentic_reformulate"); ok {
		t.Fatal("canceled caller must not start nested retrieval")
	}
	if ledger.calls != 0 {
		t.Fatalf("canceled reservation spent %d calls", ledger.calls)
	}

	expired := newRetrievalExpansionLedger(2, 1, time.Second)
	expired.deadline = time.Now().Add(-time.Millisecond)
	if _, _, ok := expired.reserve(context.Background(), "agentic_gap"); ok {
		t.Fatal("expired expansion wall budget must not start nested retrieval")
	}
	if expired.calls != 0 {
		t.Fatalf("expired reservation spent %d calls", expired.calls)
	}
	diag := map[string]any{}
	expired.stampInto(diag)
	skips := diag["retrieval_budget"].(map[string]any)["skips"].([]map[string]string)
	if !hasExpansionSkip(skips, "agentic_gap", "time_budget_exceeded") {
		t.Fatalf("time skip not diagnosed: %#v", skips)
	}

	bounded := newRetrievalExpansionLedger(1, 1, 50*time.Millisecond)
	callCtx, callCancel, ok := bounded.reserve(context.Background(), "corrective_retrieve")
	if !ok {
		t.Fatal("fresh timed budget must admit one call")
	}
	defer callCancel()
	deadline, ok := callCtx.Deadline()
	if !ok || deadline.After(bounded.deadline) {
		t.Fatalf("nested call deadline %v does not respect ledger deadline %v", deadline, bounded.deadline)
	}
}

func TestRetrievalExpansionBudgetClampsOverrides(t *testing.T) {
	t.Setenv("OUROBOROS_ERB_MAX_EXPANSION_CALLS", "999")
	t.Setenv("OUROBOROS_ERB_MAX_EXPANSION_DEPTH", "999")
	t.Setenv("OUROBOROS_ERB_EXPANSION_TIMEOUT_MS", "999999")
	ledger := newRequestRetrievalExpansionLedger()
	if ledger.maxCalls != maxRetrievalExpansionCalls ||
		ledger.maxDepth != maxRetrievalExpansionDepth ||
		ledger.timeout != maxRetrievalExpansionTimeout {
		t.Fatalf("unclamped expansion override: calls=%d depth=%d timeout=%v",
			ledger.maxCalls, ledger.maxDepth, ledger.timeout)
	}
}

func TestDefaultRetrievalExpansionBudgetKeepsBoundedRescue(t *testing.T) {
	t.Setenv("OUROBOROS_ERB_MAX_EXPANSION_CALLS", "")
	t.Setenv("OUROBOROS_ERB_MAX_EXPANSION_DEPTH", "")
	t.Setenv("OUROBOROS_ERB_EXPANSION_TIMEOUT_MS", "")
	ledger := newRequestRetrievalExpansionLedger()
	ctx := withRetrievalExpansionLedger(context.Background(), ledger)
	stages := []string{
		"exhaustive_map", "exhaustive_map", "exhaustive_map", "exhaustive_map",
		"agentic_reformulate", "agentic_reformulate", "agentic_gap",
		"corrective_retrieve",
	}
	for i, stage := range stages {
		_, cancel, ok := ledger.reserve(ctx, stage)
		if !ok {
			t.Fatalf("default budget refused bounded rescue call %d (%s)", i+1, stage)
		}
		cancel()
	}
	if _, _, ok := ledger.reserve(ctx, "extra_retrieve"); ok {
		t.Fatal("default budget admitted a ninth nested retrieval")
	}
	if ledger.calls != defaultRetrievalExpansionCalls {
		t.Fatalf("default calls = %d, want %d", ledger.calls, defaultRetrievalExpansionCalls)
	}
}

func TestAnswerBudgetStampIncludesNestedRetrievalLedger(t *testing.T) {
	ctx := withLLMLedger(context.Background(), newLLMLedger(1))
	retrieval := newRetrievalExpansionLedger(1, 1, time.Second)
	ctx = withRetrievalExpansionLedger(ctx, retrieval)
	_, cancel, ok := retrieval.reserve(ctx, "agentic_reformulate")
	if !ok {
		t.Fatal("expected nested retrieve reservation")
	}
	cancel()
	diag := map[string]any{}
	stampLLMBudget(diag, ctx)
	if _, ok := diag["llm_budget"]; !ok {
		t.Fatalf("existing #278 diagnostics missing: %#v", diag)
	}
	budget, ok := diag["retrieval_budget"].(map[string]any)
	if !ok || budget["calls"] != 1 {
		t.Fatalf("nested retrieval diagnostics missing: %#v", diag)
	}
}

func TestAgenticExpansionStopsWhenSharedCallBudgetIsSpent(t *testing.T) {
	ledger := newRetrievalExpansionLedger(1, 1, time.Second)
	ctx := withRetrievalExpansionLedger(context.Background(), ledger)
	c := &Client{cfg: Config{TopK: 4}}
	plan := QueryPlan{Completeness: true, MultiDoc: true}

	_, diag := c.agenticExpand(ctx, "List every MedThink recovery policy", "completeness", nil, AgenticOptions{
		Enabled: true, MaxRounds: 1000, MaxExtraDocs: 4, Plan: &plan,
	})
	if ledger.calls != 1 {
		t.Fatalf("agentic fan-out spent %d calls, want exactly bounded cap 1", ledger.calls)
	}
	if diag["rounds"] != 1 {
		t.Fatalf("oversized round config was not stopped after budget exhaustion: %#v", diag)
	}
	budget := diag["retrieval_budget"].(map[string]any)
	skips := budget["skips"].([]map[string]string)
	if !hasExpansionSkip(skips, "agentic_round", "call_budget_exceeded") {
		t.Fatalf("round stop was not diagnosed: %#v", skips)
	}
}

func TestExpansionRetrieveOptionsPreserveScopeWithoutGold(t *testing.T) {
	filter := map[string]any{"source_type": "slack"}
	opts := expansionRetrieveOptions(7, "basic", []string{"slack"}, filter)
	if !opts.ExpandLite || opts.TopK != 7 || opts.QuestionType != "basic" {
		t.Fatalf("unexpected expansion options: %#v", opts)
	}
	if !reflect.DeepEqual(opts.SourceTypes, []string{"slack"}) || !reflect.DeepEqual(opts.Filter, filter) {
		t.Fatalf("authorized scope not preserved: %#v", opts)
	}
	if len(opts.GoldDocIDs) != 0 {
		t.Fatalf("nested expansion must not receive eval gold IDs: %#v", opts.GoldDocIDs)
	}
}

func hasExpansionSkip(skips []map[string]string, stage, reason string) bool {
	for _, skip := range skips {
		if skip["stage"] == stage && skip["reason"] == reason {
			return true
		}
	}
	return false
}
