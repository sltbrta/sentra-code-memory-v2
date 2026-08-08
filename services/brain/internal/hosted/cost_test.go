package hosted

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
)

// unsetCostEnv clears price knobs so tests see deterministic tables.
func unsetCostEnv(t *testing.T) {
	t.Helper()
	_ = os.Unsetenv("OUROBOROS_ERB_PRICES")
}

func TestPriceTableDefaultLoads(t *testing.T) {
	unsetCostEnv(t)
	table, source, err := loadPriceTable()
	if err != nil {
		t.Fatalf("default table must load: %v", err)
	}
	if source != "default" {
		t.Fatalf("source=%q want default", source)
	}
	if len(table) == 0 {
		t.Fatal("default table must not be empty")
	}
	// The product default synth model must be priced.
	if _, ok := table.lookup("gemini", "gemini-3.6-flash"); !ok {
		t.Fatal("gemini-3.6-flash must be priced in the default table")
	}
	// Canonical-table pin: tools/erb/cost_diagnostics.py asserts the same
	// literal, so any table edit must update both sides deliberately.
	if table.digest() != "65864c42f598c59e" {
		t.Fatalf("canonical digest=%q want 65864c42f598c59e", table.digest())
	}
	// Bare-model fallback resolves unknown providers.
	if _, ok := table.lookup("mystery", "gpt-4.1-mini"); !ok {
		t.Fatal("bare model fallback must resolve gpt-4.1-mini")
	}
	if _, ok := table.lookup("mystery", "no-such-model"); ok {
		t.Fatal("unknown model must stay unpriced")
	}
}

func TestPriceDigestStableAndSensitive(t *testing.T) {
	a, err := parsePriceTable([]byte(`{"p/m":{"input_per_mtok":1,"output_per_mtok":2}}`))
	if err != nil {
		t.Fatal(err)
	}
	b, err := parsePriceTable([]byte(`{"p/m":{"output_per_mtok":2,"input_per_mtok":1}}`))
	if err != nil {
		t.Fatal(err)
	}
	if a.digest() != b.digest() {
		t.Fatal("digest must not depend on key order")
	}
	if len(a.digest()) != 16 {
		t.Fatalf("digest length=%d want 16", len(a.digest()))
	}
	c, err := parsePriceTable([]byte(`{"p/m":{"input_per_mtok":1,"output_per_mtok":3}}`))
	if err != nil {
		t.Fatal(err)
	}
	if a.digest() == c.digest() {
		t.Fatal("digest must change when prices change")
	}
	// Cross-language parity pin: tools/erb/cost_diagnostics.py must compute
	// the same digest for the same table (fixed-point encoding).
	if a.digest() != "903bb1eb0903cb94" {
		t.Fatalf("parity digest=%q want 903bb1eb0903cb94", a.digest())
	}
}

func TestPriceTableEnvOverrideReplacesDefault(t *testing.T) {
	unsetCostEnv(t)
	t.Setenv("OUROBOROS_ERB_PRICES", `{"test/model-x":{"input_per_mtok":2,"output_per_mtok":4}}`)
	table, source, err := loadPriceTable()
	if err != nil {
		t.Fatal(err)
	}
	if source != "env:OUROBOROS_ERB_PRICES" {
		t.Fatalf("source=%q want env override", source)
	}
	e, ok := table.lookup("test", "model-x")
	if !ok || e.InputPerMTok != 2 || e.OutputPerMTok != 4 {
		t.Fatalf("override entry missing or wrong: %#v", e)
	}
	// Override is a full replacement: default entries must not leak through.
	if _, ok := table.lookup("gemini", "gemini-3.6-flash"); ok {
		t.Fatal("env override must fully replace the default table")
	}
}

func TestStampLLMCostInvalidEnvFailsClosed(t *testing.T) {
	unsetCostEnv(t)
	t.Setenv("OUROBOROS_ERB_PRICES", `{not-json`)
	l := newLLMLedger(4)
	l.beginCall("synth", "primary")
	l.attempt("synth", "openai", "gpt-4.1-mini")
	l.recordUsage("synth", "openai", "gpt-4.1-mini", 100, 50, 150)
	diag := map[string]any{}
	l.stampInto(diag)
	cost, ok := diag["llm_cost"].(map[string]any)
	if !ok {
		t.Fatalf("llm_cost missing: %#v", diag)
	}
	if cost["prices_status"] != "invalid_env_json" {
		t.Fatalf("invalid env must stamp prices_status, got %#v", cost)
	}
	if _, has := cost["total_cost_usd"]; has {
		t.Fatal("invalid price config must not invent costs")
	}
	// Usage stays visible even when costing is disabled.
	if _, ok := diag["llm_usage"].(map[string]any); !ok {
		t.Fatal("llm_usage must still be stamped when costing fails closed")
	}
}

func TestStampLLMCostPricedAndUnpriced(t *testing.T) {
	unsetCostEnv(t)
	t.Setenv("OUROBOROS_ERB_PRICES",
		`{"openai/gpt-4.1-mini":{"input_per_mtok":0.4,"output_per_mtok":1.6}}`)
	l := newLLMLedger(4)
	l.beginCall("synth", "primary")
	l.attempt("synth", "openai", "gpt-4.1-mini")
	l.recordUsage("synth", "openai", "gpt-4.1-mini", 1_000_000, 500_000, 1_500_000)
	// Unpriced provider/model pair.
	l.beginCall("synth", "primary")
	l.attempt("synth", "groq", "llama-3.3-70b-versatile")
	l.recordUsage("synth", "groq", "llama-3.3-70b-versatile", 1000, 2000, 3000)

	diag := map[string]any{}
	l.stampInto(diag)
	cost, ok := diag["llm_cost"].(map[string]any)
	if !ok {
		t.Fatalf("llm_cost missing: %#v", diag)
	}
	// 1M input @0.4 + 0.5M output @1.6 = 0.4 + 0.8 = 1.2 USD.
	if got := cost["total_cost_usd"].(float64); got != 1.2 {
		t.Fatalf("total_cost_usd=%v want 1.2", got)
	}
	unpriced, ok := cost["unpriced"].([]string)
	if !ok || len(unpriced) != 1 || unpriced[0] != "groq/llama-3.3-70b-versatile" {
		t.Fatalf("unpriced list wrong: %#v", cost["unpriced"])
	}
	if digest, ok := cost["prices_digest"].(string); !ok || len(digest) != 16 {
		t.Fatalf("prices_digest missing/short: %#v", cost["prices_digest"])
	}
	if cost["currency"] != "USD" || cost["estimated"] != true {
		t.Fatalf("cost block must be explicit about currency/estimate: %#v", cost)
	}
	rows := cost["by_provider_model"].([]map[string]any)
	if len(rows) != 2 {
		t.Fatalf("by_provider_model rows=%d want 2", len(rows))
	}
}

func TestLedgerUsageAttributionAndRetries(t *testing.T) {
	l := newLLMLedger(8)
	l.beginCall("synth", "primary")
	l.attempt("synth", "gemini", "gemini-3.6-flash")
	l.recordUsage("synth", "gemini", "gemini-3.6-flash", 100, 40, 140)
	l.beginCall("completeness_retry", "thin")
	l.attempt("completeness_retry", "gemini", "gemini-3.6-flash")
	l.recordUsage("completeness_retry", "gemini", "gemini-3.6-flash", 60, 20, 0) // total derived

	diag := map[string]any{}
	l.stampInto(diag)
	usage, ok := diag["llm_usage"].(map[string]any)
	if !ok {
		t.Fatalf("llm_usage missing: %#v", diag)
	}
	if usage["input_tokens"] != 160 || usage["output_tokens"] != 60 || usage["total_tokens"] != 220 {
		t.Fatalf("usage totals wrong: %#v", usage)
	}
	if usage["retries"] != 1 || usage["calls"] != 2 || usage["attempts"] != 2 {
		t.Fatalf("usage counters wrong: %#v", usage)
	}
	byStage := usage["by_stage"].([]map[string]any)
	var retryRow map[string]any
	for _, r := range byStage {
		if r["stage"] == "completeness_retry" {
			retryRow = r
		}
	}
	if retryRow == nil || retryRow["retries"] != 1 || retryRow["input_tokens"] != 60 {
		t.Fatalf("stage attribution wrong: %#v", byStage)
	}
	byModel := usage["by_provider_model"].([]map[string]any)
	if len(byModel) != 1 || byModel[0]["provider_model"] != "gemini/gemini-3.6-flash" ||
		byModel[0]["total_tokens"] != 220 {
		t.Fatalf("provider/model attribution wrong: %#v", byModel)
	}
}

func TestLedgerMissingUsageCounted(t *testing.T) {
	l := newLLMLedger(4)
	l.beginCall("synth", "primary")
	l.attempt("synth", "openrouter", "openai/gpt-4.1-mini")
	l.recordUsage("synth", "openrouter", "openai/gpt-4.1-mini", 0, 0, 0)

	diag := map[string]any{}
	l.stampInto(diag)
	usage := diag["llm_usage"].(map[string]any)
	if usage["missing_usage_calls"] != 1 {
		t.Fatalf("missing usage not counted: %#v", usage)
	}
	// No tokens were reported: llm_tokens stays absent (honest accounting).
	if _, has := diag["llm_tokens"]; has {
		t.Fatal("llm_tokens must not appear when no usage was reported")
	}
	cost := diag["llm_cost"].(map[string]any)
	if cost["calls_missing_usage"] != 1 {
		t.Fatalf("cost block must carry missing-usage count: %#v", cost)
	}
	if got := cost["total_cost_usd"].(float64); got != 0 {
		t.Fatalf("missing usage must never be costed, got %v", got)
	}
}

// TestLedgerParallelStageAttribution guards the map-reduce fan-out shape:
// concurrent calls tagged with distinct stages must not cross-attribute.
func TestLedgerParallelStageAttribution(t *testing.T) {
	l := newLLMLedger(16)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			stage := "map_reduce_map"
			l.beginCall(stage, "facet")
			l.attempt(stage, "gemini", "gemini-3.6-flash")
			l.recordUsage(stage, "gemini", "gemini-3.6-flash", 10, 5, 15)
		}(i)
	}
	wg.Wait()
	l.beginCall("map_reduce_reduce", "merge")
	l.attempt("map_reduce_reduce", "gemini", "gemini-3.6-flash")
	l.recordUsage("map_reduce_reduce", "gemini", "gemini-3.6-flash", 100, 50, 150)

	diag := map[string]any{}
	l.stampInto(diag)
	usage := diag["llm_usage"].(map[string]any)
	byStage := usage["by_stage"].([]map[string]any)
	byKey := map[string]map[string]any{}
	for _, r := range byStage {
		byKey[r["stage"].(string)] = r
	}
	if m := byKey["map_reduce_map"]; m["calls"] != 8 || m["input_tokens"] != 80 {
		t.Fatalf("parallel map stage attribution wrong: %#v", m)
	}
	if r := byKey["map_reduce_reduce"]; r["input_tokens"] != 100 {
		t.Fatalf("reduce stage attribution wrong: %#v", r)
	}
}

func TestLLMStageContext(t *testing.T) {
	if got := llmStageFrom(context.Background()); got != "synth" {
		t.Fatalf("default stage=%q want synth", got)
	}
	ctx := withLLMStage(context.Background(), "completeness_retry")
	if got := llmStageFrom(ctx); got != "completeness_retry" {
		t.Fatalf("stage=%q want completeness_retry", got)
	}
}

// TestSynthesizeOnceStampsUsageAndCost drives one real ledger-bound synth call
// against a fake provider that reports usage, and asserts the sanitized
// llm_usage / llm_cost blocks are stamped with provider/model attribution.
func TestSynthesizeOnceStampsUsageAndCost(t *testing.T) {
	unsetBudgetEnv(t)
	unsetCostEnv(t)
	t.Setenv("OUROBOROS_ERB_PRICES",
		`{"openai/test-model":{"input_per_mtok":1,"output_per_mtok":2}}`)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{
				{"message": map[string]string{"content": `{"answer":"42","cited_document_ids":["d1"]}`}},
			},
			"usage": map[string]int{
				"prompt_tokens": 500, "completion_tokens": 100, "total_tokens": 600,
			},
		})
	}))
	defer srv.Close()
	fakeOpenAI(t, srv)
	t.Setenv("OUROBOROS_ERB_OPENAI_MODEL", "test-model")

	l := newLLMLedger(2)
	ctx := withLLMLedger(context.Background(), l)
	ctx = withLLMStage(ctx, "completeness_retry")

	_, provider, model, err := synthesizeOnce(ctx, "q", "basic",
		[]Passage{{DocumentID: "d1", Text: "evidence"}}, 400, "", nil, "")
	if err != nil {
		t.Fatalf("synthesizeOnce: %v", err)
	}
	if provider != "openai" || model != "test-model" {
		t.Fatalf("provider/model=%s/%s", provider, model)
	}

	diag := map[string]any{}
	l.stampInto(diag)
	usage, ok := diag["llm_usage"].(map[string]any)
	if !ok {
		t.Fatalf("llm_usage not stamped: %#v", diag)
	}
	if usage["input_tokens"] != 500 || usage["output_tokens"] != 100 {
		t.Fatalf("usage tokens wrong: %#v", usage)
	}
	byModel := usage["by_provider_model"].([]map[string]any)
	if len(byModel) != 1 || byModel[0]["provider_model"] != "openai/test-model" {
		t.Fatalf("provider/model attribution wrong: %#v", byModel)
	}
	byStage := usage["by_stage"].([]map[string]any)
	if len(byStage) != 1 || byStage[0]["stage"] != "completeness_retry" {
		t.Fatalf("stage attribution wrong: %#v", byStage)
	}
	cost, ok := diag["llm_cost"].(map[string]any)
	if !ok {
		t.Fatalf("llm_cost not stamped: %#v", diag)
	}
	// 500 @1/M + 100 @2/M = 0.0005 + 0.0002 = 0.0007.
	if got := cost["total_cost_usd"].(float64); got != 0.0007 {
		t.Fatalf("total_cost_usd=%v want 0.0007", got)
	}
	// Sanitized boundary: no prompt/answer/evidence text in usage/cost blocks.
	blob, _ := json.Marshal(map[string]any{"u": usage, "c": cost})
	if strings.Contains(string(blob), "evidence") {
		t.Fatal("usage/cost diagnostics must never carry evidence text")
	}
}
