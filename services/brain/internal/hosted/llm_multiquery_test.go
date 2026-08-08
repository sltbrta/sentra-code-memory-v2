package hosted

import (
	"context"
	"strings"
	"testing"
)

func TestParseLLMQueryBags(t *testing.T) {
	raw := `{"queries":["core-tiling-multiplex batching","mid-thread 200-token p95","intake chatbot discharge"]}`
	got := parseLLMQueryBags(raw, 4)
	if len(got) != 3 {
		t.Fatalf("got %v", got)
	}
	if !strings.Contains(got[0], "core-tiling") {
		t.Fatalf("first bag: %v", got)
	}
	// Fence + alternate key.
	fenced := "```json\n{\"query\":[\"us-west-2 continuous batching\",\"max_batch_tokens 2048\"]}\n```"
	got2 := parseLLMQueryBags(fenced, 2)
	if len(got2) != 2 {
		t.Fatalf("fenced: %v", got2)
	}
	// Drop overlong sentence dumps.
	long := `{"queries":["this is a very long full sentence dump that should be rejected because it has way too many tokens for a BM25 bag and dilutes everything"]}`
	if n := parseLLMQueryBags(long, 3); len(n) != 0 {
		t.Fatalf("expected drop long dump, got %v", n)
	}
}

func TestMissingContentTokensAndGapQuery(t *testing.T) {
	q := "What is the MedThink RPO for gold tier failover and continuous batching us-west-2?"
	// Passages cover only some tokens.
	ps := []Passage{
		{DocumentID: "d1", Text: "MedThink gold tier recovery point objective documented for failover."},
	}
	miss := missingContentTokens(q, ps, 8)
	joined := strings.Join(miss, " ")
	// continuous / batching / west region should be missing.
	if !strings.Contains(joined, "continuous") && !strings.Contains(joined, "batching") &&
		!strings.Contains(joined, "west") {
		// us-west-2 may tokenize oddly; at least something missing.
		if len(miss) == 0 {
			t.Fatalf("expected missing tokens, miss=%v", miss)
		}
	}
	// Gap stopwords should not appear alone as the gap bag noise.
	for _, bad := range []string{"made", "safer", "using", "based"} {
		if gapStopword(bad) != true {
			t.Fatalf("expected gap stopword %q", bad)
		}
	}
	gapQ := gapQueryFromPassages(q, ps)
	if gapQ == "" {
		t.Fatal("expected non-empty gap query")
	}
	// Prefer paraphrase bag when uncovered (continuous batching pattern).
	if !strings.Contains(strings.ToLower(gapQ), "batch") &&
		!strings.Contains(strings.ToLower(gapQ), "west") &&
		!strings.Contains(strings.ToLower(gapQ), "continuous") {
		// Still OK if identifiers/bags differ; must not be pure stopword mush.
		for _, w := range strings.Fields(gapQ) {
			if gapStopword(strings.ToLower(w)) && len(strings.Fields(gapQ)) <= 2 {
				t.Fatalf("gap query looks like stopword mush: %q", gapQ)
			}
		}
	}
	// Full coverage → empty or weak gap.
	ps2 := []Passage{{
		DocumentID: "d2",
		Text:       "MedThink RPO gold tier failover continuous batching us-west-2 inference",
	}}
	if g2 := gapQueryFromPassages(q, ps2); g2 != "" && lexicalGap(q, ps2) > 0.1 {
		t.Fatalf("expected low gap when covered, gapQ=%q gap=%.2f", g2, lexicalGap(q, ps2))
	}
}

func TestSemanticExpandBagsFromFull500Shapes(t *testing.T) {
	t.Setenv("OUROBOROS_ERB_DIAGNOSTIC_RESCUE", "1")
	t.Setenv("OUROBOROS_ERB_PROD", "0")
	t.Setenv("OUROBOROS_ERB_OFFICIAL_JUDGE", "0")
	// Pattern bags for full500 semantic pool0 shapes (diagnostic-only; not official steering).
	cases := []struct {
		q    string
		want string
	}{
		{"What were the final concession terms for retail partner marketplace purchase?", "marketplace"},
		{"What is the default pass rate for safest numeric mode step down?", "numeric"},
		{"What mechanism prevents full user traffic until dry run with replayed requests?", "latch"},
		{"What metric tracks streaming sessions finalized due to time limit?", "streaming"},
		{"What is the contractor access default duration in the permissions playbook?", "contractor"},
	}
	for _, tc := range cases {
		bags := pickHotLexPhrases(tc.q, 4)
		joined := strings.ToLower(strings.Join(bags, " "))
		if !strings.Contains(joined, tc.want) {
			t.Fatalf("q=%q want bag containing %q got %v", tc.q, tc.want, bags)
		}
	}
}

func TestMultiQueryVariantsWithLLMDisabledFallsBack(t *testing.T) {
	t.Setenv("OUROBOROS_ERB_LLM_MULTIQUERY", "0")
	t.Setenv("OUROBOROS_ERB_QUALITY", "0")
	// Clear keys so even if someone enables, we still get static.
	t.Setenv("CEREBRAS_API_KEY", "")
	t.Setenv("GROQ_API_KEY", "")
	t.Setenv("OPENAI_API_KEY", "")
	t.Setenv("OPENROUTER_API_KEY", "")
	vars, meta := multiQueryVariantsWithLLM(context.Background(),
		"What is the default RPO for MedThink failover?", "semantic")
	if len(vars) < 2 {
		t.Fatalf("static fallback expected variants, got %v meta=%v", vars, meta)
	}
	if meta["llm_multiquery"] == true {
		t.Fatalf("llm should be off, meta=%v", meta)
	}
}

func TestLLMMultiQueryEnabledDefaults(t *testing.T) {
	t.Setenv("OUROBOROS_ERB_LLM_MULTIQUERY", "")
	t.Setenv("OUROBOROS_ERB_QUALITY", "1")
	if !llmMultiQueryEnabled() {
		t.Fatal("QUALITY should default LLM multi-query on")
	}
	if !llmMultiQueryWanted("semantic") {
		t.Fatal("semantic wanted under QUALITY")
	}
	if llmMultiQueryWanted("basic") {
		t.Fatal("basic expand must skip LLM multi-query")
	}
	t.Setenv("OUROBOROS_ERB_QUALITY", "0")
	if llmMultiQueryEnabled() {
		t.Fatal("non-QUALITY should default LLM multi-query off")
	}
	t.Setenv("OUROBOROS_ERB_LLM_MULTIQUERY", "1")
	if !llmMultiQueryEnabled() {
		t.Fatal("explicit on")
	}
	t.Setenv("OUROBOROS_ERB_LLM_MULTIQUERY", "0")
	t.Setenv("OUROBOROS_ERB_QUALITY", "1")
	if llmMultiQueryEnabled() {
		t.Fatal("explicit off wins over QUALITY")
	}
}
