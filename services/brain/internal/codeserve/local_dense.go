package codeserve

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/codecrawl"
	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/denselocal"
)

// handleDenseLocal implements the codeserve side of the optional local-only
// lexical retrieval arm (issue #59). All work is local; no network call is
// made and no defaults change for callers that don't invoke this verb.
//
// Wire shape:
//
//	verb: dense_local_search
//	root: source root (must contain eligible source files)
//	q: query (required)
//	top_k: maximum hits (clamped to denselocal.DefaultBounds.MaxTopK)
//	scope: identity scope label (default: canonical root)
//	model: identity model label (default bag-of-words:v1)
//	max_corpus / max_dim / max_query_len: bound tightenings; a request can
//	  only narrow the hard default ceilings, never raise them
func handleDenseLocal(ctx context.Context, req Request) Response {
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
	// Hard bounds: the defaults are ceilings. A request may tighten them but
	// can never raise them, so resource envelopes stay operator-controlled.
	bounds := denselocal.DefaultBounds()
	var ok bool
	if bounds.MaxCorpus, ok = tightenBound(req["max_corpus"], bounds.MaxCorpus); !ok {
		return denseBoundError()
	}
	if bounds.MaxDim, ok = tightenBound(req["max_dim"], bounds.MaxDim); !ok {
		return denseBoundError()
	}
	if bounds.MaxQueryLen, ok = tightenBound(req["max_query_len"], bounds.MaxQueryLen); !ok {
		return denseBoundError()
	}
	topK := intOf(req["top_k"], 10)
	if topK <= 0 {
		return denseBoundError()
	}
	if topK > bounds.MaxTopK {
		topK = bounds.MaxTopK
	}
	scope, _ := req["scope"].(string)
	if strings.TrimSpace(scope) == "" {
		scope = root
	}
	modelName, _ := req["model"].(string)
	if modelName == "" {
		modelName = string(denselocal.ModelBag)
	}
	bagTexts, err := loadLocalBagTexts(ctx, root, bounds.MaxCorpus)
	if err != nil {
		return Response{
			"ok": false, "verb": string(VerbDenseLocal),
			"error":      err.Error(),
			"error_code": string(ErrInternal),
		}
	}
	if len(bagTexts) == 0 {
		// Empty corpus is explicit, never a silent zero-hit success.
		return Response{
			"ok": false, "verb": string(VerbDenseLocal),
			"error":      "no eligible source files under root",
			"error_code": string(ErrInvalidRequest),
		}
	}
	eng, err := denselocal.NewEngine(scope, denselocal.Model(modelName), bagTexts, bounds)
	if err != nil {
		return Response{
			"ok": false, "verb": string(VerbDenseLocal),
			"error":      err.Error(),
			"error_code": string(ErrInternal),
		}
	}
	report, err := eng.Search(ctx, q, denselocal.Options{
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
		"identity_checked": report.IdentityChecked,
		"hits":             report.Hits,
		"query_sha256":     report.Query,
		"model":            report.Model,
		"scope":            report.Scope,
		"bounded_by":       report.BoundedBy,
	})
	if raw, mErr := json.Marshal(report); mErr == nil {
		out["report"] = json.RawMessage(raw)
	}
	return out
}

// tightenBound resolves one caller-supplied bound against its hard default
// ceiling. Absent or zero keeps the ceiling; larger values clamp down to the
// ceiling; negative values are rejected (ok=false).
func tightenBound(v any, ceiling int) (int, bool) {
	if v == nil {
		return ceiling, true
	}
	n := intOf(v, -1)
	if n < 0 {
		return 0, false
	}
	if n == 0 || n > ceiling {
		return ceiling, true
	}
	return n, true
}

func denseBoundError() Response {
	return Response{
		"ok": false, "verb": string(VerbDenseLocal),
		"error":      "bounds must be positive",
		"error_code": string(ErrInvalidRequest),
	}
}

// loadLocalBagTexts reads up to maxFiles eligible source files under root,
// each capped by readFileBounded, so corpus loading is bounded in both file
// count and per-file bytes before any search work begins. An unreadable root
// reports as an empty corpus (the handler turns that into an explicit
// error); context cancellation aborts the walk early.
func loadLocalBagTexts(ctx context.Context, root string, maxFiles int) (map[string]string, error) {
	files, err := codecrawl.SourceFiles(root)
	if err != nil {
		return nil, nil
	}
	out := make(map[string]string, min(len(files), maxFiles))
	for i, abs := range files {
		if len(out) >= maxFiles {
			break
		}
		if i%64 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		if data, err := readFileBounded(abs, 6<<10); err == nil && len(data) > 0 {
			out[abs] = string(data)
		}
	}
	return out, nil
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
