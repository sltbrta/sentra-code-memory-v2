package hosted

import (
	"context"
	"os"
	"strings"

	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/llmadapter"
)

// llmadapter had 727 lines and no non-test importer.
//
// It is a second implementation of query expansion and candidate scoring,
// including a Gemini provider seam, sitting beside the one in llm_multiquery.go
// that is actually used. Two implementations of the same capability, one of
// them never run, is how a package stops being maintained and starts being
// believed: its README describes behaviour nothing exercises.
//
// It is now reachable, behind an explicit opt-in that is off by default:
//
//	OUROBOROS_BRAIN_LLMADAPTER_EXPAND=1
//
// Off, nothing changes, which is what every existing test depends on. On and
// unconfigured, llmadapter's own deterministic fallback returns the expansions
// -- it abstains rather than fabricating, so an absent API key degrades to the
// existing behaviour rather than to an error.
//
// The prompt framing was fixed before this was wired, not after. Every prompt
// in the package concatenated repository content straight into the
// instruction; wiring a consumer to that would have taken a dormant defect and
// made it reachable.

// llmadapterExpandEnvVar is the explicit opt-in. It is off by default because
// this path has never run in production, and turning on an unexercised code
// path by default is how a dormant package becomes an incident.
const llmadapterExpandEnvVar = "OUROBOROS_BRAIN_LLMADAPTER_EXPAND"

// llmadapterExpansionEnabled reports the opt-in.
func llmadapterExpansionEnabled() bool {
	return envTruthy(llmadapterExpandEnvVar, false)
}

// llmadapterExpandQueries expands a question through llmadapter.
//
// The returned meta records which path ran and why, so a deployment that
// enabled the opt-in can tell whether it took effect rather than inferring it
// from retrieval quality.
func llmadapterExpandQueries(ctx context.Context, question string, maxN int) ([]string, map[string]any) {
	meta := map[string]any{"llmadapter_expand": false}
	if !llmadapterExpansionEnabled() {
		meta["skip"] = "disabled"
		return nil, meta
	}
	question = strings.TrimSpace(question)
	if question == "" {
		meta["skip"] = "empty_question"
		return nil, meta
	}
	if maxN <= 0 {
		maxN = 4
	}

	cfg := llmadapter.Config{
		APIKey:        strings.TrimSpace(os.Getenv("GEMINI_API_KEY")),
		MaxExpansions: maxN,
	}
	// A nil generator is the unconfigured case, and llmadapter answers it with
	// its deterministic fallback rather than an error. That is why this is
	// wired to the constructor that tolerates one: an opt-in that fails when a
	// key is absent would be an opt-in nobody could safely turn on.
	var generator llmadapter.Generator
	if gemini, err := llmadapter.NewGeminiGenerator(ctx, cfg); err == nil {
		generator = gemini
	} else {
		meta["provider"] = "none"
	}
	service := llmadapter.New(cfg, generator)

	queries, diag := service.ExpandQuery(ctx, question)
	meta["llmadapter_expand"] = diag.LLMUsed
	if diag.FallbackReason != "" {
		meta["fallback"] = diag.FallbackReason
	}
	if diag.Provider != "" {
		meta["provider"] = diag.Provider
		meta["model"] = diag.Model
	}
	return queries, meta
}
