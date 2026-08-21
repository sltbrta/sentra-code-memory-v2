package hosted

import (
	"context"
	"testing"
)

// llmadapter had 727 lines and no non-test importer: a second implementation
// of query expansion, including a Gemini provider seam, sitting beside the one
// that is actually used. It is now reachable behind an explicit opt-in.
//
// The opt-in is off by default because this path has never run in production,
// and turning on an unexercised code path by default is how a dormant package
// becomes an incident.

func TestLLMAdapterExpansionIsOffByDefault(t *testing.T) {
	t.Setenv("OUROBOROS_BRAIN_LLMADAPTER_EXPAND", "")
	if llmadapterExpansionEnabled() {
		t.Fatal("the opt-in is on with no environment variable set")
	}
	queries, meta := llmadapterExpandQueries(context.Background(), "find the auth path", 4)
	if len(queries) != 0 {
		t.Fatalf("a disabled path produced expansions: %v", queries)
	}
	if meta["skip"] != "disabled" {
		t.Fatalf("the skip reason does not say it is disabled: %+v", meta)
	}
}

// TestLLMAdapterExpansionFallsBackWithoutCredentials is what makes the opt-in
// safe to turn on. Enabled and unconfigured, llmadapter's own deterministic
// fallback answers, so an absent API key degrades to expansions rather than to
// an error.
func TestLLMAdapterExpansionFallsBackWithoutCredentials(t *testing.T) {
	t.Setenv("OUROBOROS_BRAIN_LLMADAPTER_EXPAND", "1")
	t.Setenv("GEMINI_API_KEY", "")

	queries, meta := llmadapterExpandQueries(context.Background(), "validate session token", 4)
	if used, _ := meta["llmadapter_expand"].(bool); used {
		t.Fatalf("no provider is configured, so no model call should have happened: %+v", meta)
	}
	if meta["fallback"] == nil {
		t.Fatalf("the fallback reason is not reported, so a deployment cannot "+
			"tell whether the opt-in took effect: %+v", meta)
	}
	if len(queries) == 0 {
		t.Fatal("the deterministic fallback returned nothing: enabling the " +
			"opt-in without a key would silently narrow retrieval")
	}
	for _, q := range queries {
		if q == "" {
			t.Fatal("an empty expansion was returned")
		}
	}
}

// TestExpansionIsUnchangedWhenTheOptInIsOff pins the property every existing
// test depends on: with the opt-in off, the ordinary path runs as before.
func TestExpansionIsUnchangedWhenTheOptInIsOff(t *testing.T) {
	t.Setenv("OUROBOROS_BRAIN_LLMADAPTER_EXPAND", "")
	ctx := context.Background()
	const question = "how does the warm index refresh work"

	withAdapter, _ := multiQueryVariantsWithLLM(ctx, question, "semantic")
	static := multiQueryVariants(question, "semantic")

	if len(withAdapter) < len(static) {
		t.Fatalf("the disabled opt-in lost variants: %d vs %d static",
			len(withAdapter), len(static))
	}
	for _, want := range static {
		var found bool
		for _, got := range withAdapter {
			if got == want {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("static variant %q was dropped by the disabled opt-in", want)
		}
	}
}

// TestEnabledExpansionStillIncludesTheStaticVariants keeps the opt-in from
// narrowing retrieval: whatever the adapter returns is added to the
// deterministic variants, never substituted for them.
func TestEnabledExpansionStillIncludesTheStaticVariants(t *testing.T) {
	t.Setenv("OUROBOROS_BRAIN_LLMADAPTER_EXPAND", "1")
	t.Setenv("GEMINI_API_KEY", "")
	ctx := context.Background()
	const question = "how does the warm index refresh work"

	got, meta := multiQueryVariantsWithLLM(ctx, question, "semantic")
	for _, want := range multiQueryVariants(question, "semantic") {
		var found bool
		for _, have := range got {
			if have == want {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("static variant %q was dropped when the opt-in was on "+
				"(meta=%+v)", want, meta)
		}
	}
}
