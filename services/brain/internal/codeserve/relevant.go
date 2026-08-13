package codeserve

import (
	"time"

	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/codecrawl"
	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/contextpack"
)

func handleFindRelevant(req Request) Response {
	q := str(req, "q")
	if q == "" {
		return errResp(string(VerbFindRelevant), "q required")
	}
	maxBytes := intField(req, "max_bytes", 0)
	maxTokens := intField(req, "max_tokens", 0)
	renderStr := str(req, "render")
	bounded := maxBytes > 0 || maxTokens > 0 || renderStr != ""
	if renderStr != "" && maxBytes <= 0 && maxTokens <= 0 {
		maxBytes = 256 << 10
	}
	mode := contextpack.RenderFull
	if bounded {
		var err error
		mode, err = contextpack.ParseRenderMode(renderStr)
		if err != nil {
			return errResp(string(VerbFindRelevant), err.Error())
		}
	}
	idx, rootAbs, _, err := loadIndex(req)
	if err != nil {
		return idxErrResp(string(VerbFindRelevant), err)
	}
	topK := intField(req, "top_k", 5)
	if topK > findRelevantCandidateCap {
		topK = findRelevantCandidateCap
	}
	preview := boolField(req, "preview", true)
	ranked := boolField(req, "ranked", false)
	t0 := time.Now()
	payload := idx.FindRelevant(rootAbs, q, topK, preview)
	packingPayload := payload
	out := map[string]any{
		"payload":        payload,
		"search_backend": "codecrawl",
	}
	if ranked {
		rankedPayload := idx.FindRelevantRanked(rootAbs, q, topK, preview, codecrawl.DefaultRankerConfig())
		out["ranked_payload"] = rankedPayload
		packingPayload = rankedPayload.AgentPayload
		if boolField(req, "impact_tests", false) {
			rec := idx.Impact(q, 3, 64)
			affectedTests := rec.AffectedTests
			if affectedTests == nil {
				affectedTests = []string{}
			}
			out["affected_tests"] = affectedTests
		}
	}
	if bounded {
		out["context"] = packFindRelevant(req, rootAbs, packingPayload, mode, maxBytes, maxTokens)
	}
	out["duration_ms"] = time.Since(t0).Milliseconds()
	return okResp(string(VerbFindRelevant), out)
}
