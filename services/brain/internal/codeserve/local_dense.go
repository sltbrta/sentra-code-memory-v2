package codeserve

import (
	"encoding/json"
	"strings"

	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/codecrawl"
	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/denselocal"
)

// handleDenseLocal implements the codeserve side of the optional local-only
// dense retrieval arm (issue #59). All work is local; no network call is
// made and no defaults change for callers that don't invoke this verb.
//
// Wire shape:
//
//	verb: dense_local_search
//	root: source root (must contain eligible source files)
//	q: query (required)
//	top_k: maximum hits (bounded by denselocal.DefaultBounds.MaxTopK)
//	scope: identity scope (default: canonical root)
//	model: identity model name (default bag-of-words:v1)
//	index: optional path to a persisted HNSW index for identity-bound lookup
//	max_corpus / max_dim / max_query_len / top_k: bounds overrides
func handleDenseLocal(req Request) Response {
	q, _ := req["q"].(string)
	if strings.TrimSpace(q) == "" {
		return Response{
			"ok": false, "verb": string(VerbDenseLocal),
			"error":      "q is required",
			"error_code": string(ErrInvalidRequest),
		}
	}
	root, _ := req["root"].(string)
	if root == "" {
		return Response{
			"ok": false, "verb": string(VerbDenseLocal),
			"error":      "root is required",
			"error_code": string(ErrInvalidRequest),
		}
	}
	topK := intOf(req["top_k"], 10)
	maxCorpus := intOf(req["max_corpus"], denselocal.DefaultBounds().MaxCorpus)
	maxDim := intOf(req["max_dim"], denselocal.DefaultBounds().MaxDim)
	maxQuery := intOf(req["max_query_len"], denselocal.DefaultBounds().MaxQueryLen)
	if maxCorpus <= 0 || maxDim <= 0 || topK <= 0 || maxQuery <= 0 {
		return Response{
			"ok": false, "verb": string(VerbDenseLocal),
			"error":      "bounds must be positive",
			"error_code": string(ErrInvalidRequest),
		}
	}
	scope, _ := req["scope"].(string)
	if strings.TrimSpace(scope) == "" {
		scope = root
	}
	modelName, _ := req["model"].(string)
	if modelName == "" {
		modelName = string(denselocal.ModelBag)
	}
	indexPath, _ := req["index"].(string)
	bagTexts := loadLocalBagTexts(root)
	if len(bagTexts) == 0 {
		// Empty corpus is fine; the lexical path returns no hits rather than
		// erroring. We still report ok=false to make the absence explicit.
		return Response{
			"ok": false, "verb": string(VerbDenseLocal),
			"error":      "no eligible source files under root",
			"error_code": string(ErrIndexUnavailable),
		}
	}
	bounds := denselocal.Bounds{
		MaxCorpus: maxCorpus, MaxDim: maxDim,
		MaxTopK: topK, MaxQueryLen: maxQuery,
	}
	eng, err := denselocal.NewEngine(scope, denselocal.Model(modelName), 0, nil, bagTexts, bounds)
	if err != nil {
		return Response{
			"ok": false, "verb": string(VerbDenseLocal),
			"error":      err.Error(),
			"error_code": string(ErrInternal),
		}
	}
	if indexPath != "" {
		if err := eng.LoadIndex(indexPath, denselocal.Options{
			Scope: scope, Model: denselocal.Model(modelName),
			TopK: topK, Bounds: bounds,
		}); err != nil {
			return Response{
				"ok": false, "verb": string(VerbDenseLocal),
				"error":      err.Error(),
				"error_code": string(ErrIndexUnavailable),
			}
		}
	}
	report, err := eng.Search(reqCtx(), q, denselocal.Options{
		Scope: scope, Model: denselocal.Model(modelName),
		TopK: topK, Bounds: bounds,
	})
	if err != nil {
		return Response{
			"ok": false, "verb": string(VerbDenseLocal),
			"error":      err.Error(),
			"error_code": string(ErrInvalidRequest),
		}
	}
	out := okResp(string(VerbDenseLocal), map[string]any{
		"route":            report.Route,
		"index_state":      report.IndexState,
		"identity_checked": report.IdentityChecked,
		"hits":             report.Hits,
		"corpus_vectors":   report.CorpusVectors,
		"query_sha256":     report.Query,
		"model":            report.Model,
		"scope":            report.Scope,
		"content_digest":   report.ContentDigest,
		"bounded_by":       report.BoundedBy,
	})
	if raw, mErr := json.Marshal(report); mErr == nil {
		out["report"] = json.RawMessage(raw)
	}
	return out
}

func loadLocalBagTexts(root string) map[string]string {
	files, err := codecrawl.SourceFiles(root)
	if err != nil {
		return nil
	}
	out := make(map[string]string, len(files))
	for _, abs := range files {
		if data, err := readFileBounded(abs, 6<<10); err == nil && len(data) > 0 {
			out[abs] = string(data)
		}
	}
	return out
}

func intOf(v any, fallback int) int {
	switch t := v.(type) {
	case int:
		return t
	case int64:
		return int(t)
	case float64:
		return int(t)
	case string:
		var n int
		_, _ = fmtParseInt(t, &n)
		if n > 0 {
			return n
		}
	}
	return fallback
}
