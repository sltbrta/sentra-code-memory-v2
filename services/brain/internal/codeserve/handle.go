package codeserve

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/codecrawl"
	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/hosted"
	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/productsearch"
)

// Request is one JSON-line protocol request.
type Request map[string]any

// Response is one JSON-line protocol response.
type Response map[string]any

// Handle dispatches one multi-verb request. Never panics; always returns a map.
func Handle(ctx context.Context, req Request) Response {
	if req == nil {
		return errResp("", "nil request")
	}
	verb, _ := req["verb"].(string)
	verb = strings.TrimSpace(verb)
	switch Verb(verb) {
	case VerbPing:
		return okResp(verb, map[string]any{"product_owned": true})
	case VerbCatalog, "":
		if verb == "" {
			verb = string(VerbCatalog)
		}
		extra := map[string]any{
			"verbs": Catalog(), "contract": ContractID,
			"product_owned": true,
		}
		// Specs are opt-in to keep the discovery response lean.
		if boolField(req, "detail", false) || boolField(req, "specs", false) {
			extra["specs"] = CatalogMetadata()
		}
		return okResp(verb, extra)
	case VerbCodeIndex:
		return handleCodeIndex(req)
	case VerbCodeSearch:
		return handleCodeSearch(req)
	case VerbFindRelevant:
		return handleFindRelevant(req)
	case VerbExpand:
		return handleExpand(req)
	case VerbImpact:
		return handleImpact(req)
	case VerbFindRoute:
		return handleFindRoute(req)
	case VerbFreshness:
		return handleFreshness(req)
	case VerbIngestPaths:
		return handleIngestPaths(req)
	case VerbCodeExact, VerbCodeDefs, VerbCodeRefs:
		return handleCodeExact(ctx, req, Verb(verb))
	case VerbMemoryAsk:
		return handleMemoryAsk(ctx, req)
	// Back-compat alias used by earlier serve path.
	case "find_route":
		req["verb"] = string(VerbFindRoute)
		return handleFindRoute(req)
	default:
		return Response{
			"ok": false, "verb": verb, "error": "unknown verb",
			"error_code": string(ErrUnknownVerb), "product_owned": true,
			"supported": Catalog(),
		}
	}
}

func okResp(verb string, extra map[string]any) Response {
	out := Response{"ok": true, "verb": verb, "product_owned": true}
	for k, v := range extra {
		out[k] = v
	}
	return out
}

func codeErrResp(verb string, code ErrorCode, msg string) Response {
	return Response{
		"ok": false, "verb": verb, "error": msg,
		"error_code": string(code), "product_owned": true,
	}
}

func errResp(verb, msg string) Response {
	return codeErrResp(verb, ErrInvalidRequest, msg)
}

// idxErrResp classifies durable-index load/refresh failures.
func idxErrResp(verb string, err error) Response {
	return codeErrResp(verb, ErrIndexUnavailable, err.Error())
}

func str(req Request, key string) string {
	v, _ := req[key].(string)
	return strings.TrimSpace(v)
}

func intField(req Request, key string, def int) int {
	switch v := req[key].(type) {
	case float64:
		return int(v)
	case int:
		return v
	case int64:
		return int(v)
	default:
		return def
	}
}

func boolField(req Request, key string, def bool) bool {
	switch v := req[key].(type) {
	case bool:
		return v
	default:
		return def
	}
}

func resolvePaths(req Request) (rootAbs, gobPath string, err error) {
	root := str(req, "root")
	cache := str(req, "index_cache")
	if cache == "" {
		cache = str(req, "index-cache")
	}
	if root == "" && cache == "" {
		return "", "", fmt.Errorf("root or index_cache required")
	}
	if root != "" {
		rootAbs, err = filepath.Abs(root)
		if err != nil {
			return "", "", err
		}
	}
	if cache != "" {
		out, err := filepath.Abs(cache)
		if err != nil {
			return "", "", err
		}
		gobPath = filepath.Join(out, "code-index.gob")
		if rootAbs == "" {
			// Load-only path: root from index meta after Load.
			return "", gobPath, nil
		}
		return rootAbs, gobPath, nil
	}
	gobPath = filepath.Join(rootAbs, ".ouroboros", "code-index.gob")
	return rootAbs, gobPath, nil
}

func loadIndex(req Request) (*codecrawl.Index, string, string, error) {
	rootAbs, gobPath, err := resolvePaths(req)
	if err != nil {
		return nil, "", "", err
	}
	workers := intField(req, "workers", 4)
	if workers <= 0 {
		workers = 4
	}
	noRefresh := boolField(req, "no_refresh", false)
	force := boolField(req, "force", false)
	if noRefresh {
		idx, meta, err := codecrawl.Load(gobPath)
		if err != nil {
			return nil, "", "", err
		}
		if rootAbs == "" {
			rootAbs = meta.Root
		}
		return idx, rootAbs, gobPath, nil
	}
	if rootAbs == "" {
		return nil, "", "", fmt.Errorf("root required unless no_refresh with existing gob")
	}
	idx, _, _, _, err := codecrawl.OpenOrRefresh(rootAbs, gobPath, workers, force)
	if err != nil {
		return nil, "", "", err
	}
	return idx, rootAbs, gobPath, nil
}

func handleCodeIndex(req Request) Response {
	rootAbs, gobPath, err := resolvePaths(req)
	if err != nil || rootAbs == "" {
		return errResp(string(VerbCodeIndex), "root required")
	}
	workers := intField(req, "workers", 4)
	force := boolField(req, "force", false)
	t0 := time.Now()
	idx, st, wrote, meta, err := codecrawl.OpenOrRefresh(rootAbs, gobPath, workers, force)
	if err != nil {
		return idxErrResp(string(VerbCodeIndex), err)
	}
	_ = idx
	return okResp(string(VerbCodeIndex), map[string]any{
		"root": rootAbs, "gob_path": gobPath, "wrote": wrote,
		"changed": st.Changed, "unchanged": st.Unchanged,
		"skipped_by_stamp": st.SkippedByStamp,
		"duration_ms":      time.Since(t0).Milliseconds(),
		"git_head":         meta.GitHead,
		"search_backend":   "codecrawl",
	})
}

func handleCodeSearch(req Request) Response {
	q := str(req, "q")
	if q == "" {
		return errResp(string(VerbCodeSearch), "q required")
	}
	idx, rootAbs, gobPath, err := loadIndex(req)
	if err != nil {
		return idxErrResp(string(VerbCodeSearch), err)
	}
	topK := intField(req, "top_k", 8)
	if topK <= 0 {
		topK = 8
	}
	t0 := time.Now()
	hits := idx.SearchOpts(q, topK, true)
	return okResp(string(VerbCodeSearch), map[string]any{
		"root": rootAbs, "gob_path": gobPath, "q": q, "hits": hits,
		"duration_ms": time.Since(t0).Milliseconds(), "search_backend": "codecrawl",
	})
}

func handleFindRelevant(req Request) Response {
	q := str(req, "q")
	if q == "" {
		return errResp(string(VerbFindRelevant), "q required")
	}
	idx, rootAbs, _, err := loadIndex(req)
	if err != nil {
		return idxErrResp(string(VerbFindRelevant), err)
	}
	topK := intField(req, "top_k", 5)
	preview := boolField(req, "preview", true)
	t0 := time.Now()
	payload := idx.FindRelevant(rootAbs, q, topK, preview)
	return okResp(string(VerbFindRelevant), map[string]any{
		"payload": payload, "duration_ms": time.Since(t0).Milliseconds(),
		"search_backend": "codecrawl",
	})
}

func handleExpand(req Request) Response {
	seed := str(req, "seed")
	if seed == "" {
		seed = str(req, "q")
	}
	if seed == "" {
		return errResp(string(VerbExpand), "seed required")
	}
	idx, _, _, err := loadIndex(req)
	if err != nil {
		return idxErrResp(string(VerbExpand), err)
	}
	// Expand via defs+refs of seed symbol (heuristic).
	defs := idx.DefsOf(seed)
	refs := idx.RefsOf(seed)
	return okResp(string(VerbExpand), map[string]any{
		"seed": seed, "defs": defs, "refs": refs,
		"authority": "heuristic", "search_backend": "codecrawl",
	})
}

func handleImpact(req Request) Response {
	seed := str(req, "seed")
	if seed == "" {
		seed = str(req, "q")
	}
	if seed == "" {
		return errResp(string(VerbImpact), "seed required")
	}
	idx, _, _, err := loadIndex(req)
	if err != nil {
		return idxErrResp(string(VerbImpact), err)
	}
	depth := intField(req, "max_depth", 3)
	maxFiles := intField(req, "max_files", 64)
	rec := idx.Impact(seed, depth, maxFiles)
	// Normalize authority label for honesty (SCM-006).
	if rec.Authority == "" || rec.Authority == "heuristic_plus" {
		rec.Authority = "heuristic"
	}
	return okResp(string(VerbImpact), map[string]any{
		"receipt": rec, "search_backend": "codecrawl",
	})
}

func handleFindRoute(req Request) Response {
	from := str(req, "from")
	to := str(req, "to")
	if from == "" || to == "" {
		return errResp(string(VerbFindRoute), "from and to required")
	}
	// Prefer warm gob when possible (latency): default no full crawl if gob exists.
	if _, ok := req["no_refresh"]; !ok {
		req["no_refresh"] = true
	}
	idx, _, _, err := loadIndex(req)
	if err != nil {
		// Fall back to refresh if gob missing.
		req["no_refresh"] = false
		idx, _, _, err = loadIndex(req)
		if err != nil {
			return idxErrResp(string(VerbFindRoute), err)
		}
	}
	maxB := intField(req, "max_bridges", 12)
	rec := idx.FindRoute(from, to, maxB)
	return okResp(string(VerbFindRoute), map[string]any{
		"receipt": rec, "search_backend": "codecrawl",
	})
}

func handleFreshness(req Request) Response {
	rootAbs, gobPath, err := resolvePaths(req)
	if err != nil {
		return errResp(string(VerbFreshness), err.Error())
	}
	idx, meta, err := codecrawl.Load(gobPath)
	if err != nil {
		return idxErrResp(string(VerbFreshness), err)
	}
	if rootAbs == "" {
		rootAbs = meta.Root
	}
	t0 := time.Now()
	rep := idx.Freshness(rootAbs, meta)
	rep.GobPath = gobPath
	return okResp(string(VerbFreshness), map[string]any{
		"report": rep, "duration_ms": time.Since(t0).Milliseconds(),
	})
}

func handleIngestPaths(req Request) Response {
	rootAbs, gobPath, err := resolvePaths(req)
	if err != nil || rootAbs == "" {
		return errResp(string(VerbIngestPaths), "root required")
	}
	var rels []string
	switch p := req["paths"].(type) {
	case string:
		for _, s := range strings.Split(p, ",") {
			s = strings.TrimSpace(s)
			if s != "" {
				rels = append(rels, s)
			}
		}
	case []any:
		for _, x := range p {
			if s, ok := x.(string); ok && strings.TrimSpace(s) != "" {
				rels = append(rels, strings.TrimSpace(s))
			}
		}
	}
	if len(rels) == 0 {
		return errResp(string(VerbIngestPaths), "paths required")
	}
	idx, meta, err := codecrawl.Load(gobPath)
	if err != nil {
		idx, _, _, meta, err = codecrawl.OpenOrRefresh(rootAbs, gobPath, 4, true)
		if err != nil {
			return idxErrResp(string(VerbIngestPaths), err)
		}
	}
	n, err := idx.IngestPaths(rootAbs, rels)
	if err != nil {
		return codeErrResp(string(VerbIngestPaths), ErrInternal, err.Error())
	}
	if err := idx.Save(gobPath, rootAbs); err != nil {
		return codeErrResp(string(VerbIngestPaths), ErrInternal, err.Error())
	}
	return okResp(string(VerbIngestPaths), map[string]any{
		"changed": n, "paths": rels, "gob_path": gobPath, "root": rootAbs,
		"git_head": meta.GitHead,
	})
}

func handleCodeExact(ctx context.Context, req Request, verb Verb) Response {
	root := str(req, "root")
	q := str(req, "q")
	if q == "" {
		q = str(req, "symbol")
	}
	if root == "" || q == "" {
		return errResp(string(verb), "root and q required")
	}
	kind := str(req, "kind")
	if verb == VerbCodeDefs {
		kind = "definition"
	}
	if verb == VerbCodeRefs {
		kind = "reference"
	}
	topK := intField(req, "top_k", 20)
	t0 := time.Now()
	res := productsearch.Search(ctx, productsearch.Request{
		Profile: productsearch.ProfileCodeExact, CodeRoot: root, Question: q,
		TopK: topK, ExactKind: kind,
	})
	return okResp(string(verb), map[string]any{
		"result": res, "duration_ms": time.Since(t0).Milliseconds(),
		"search_backend": "codeindex",
	})
}

func handleMemoryAsk(ctx context.Context, req Request) Response {
	dir := str(req, "dir")
	q := str(req, "q")
	if dir == "" || q == "" {
		return errResp(string(VerbMemoryAsk), "dir and q required")
	}
	c, err := hosted.OpenLocal(dir, "local")
	if err != nil {
		return errResp(string(VerbMemoryAsk), err.Error())
	}
	defer func() { _ = c.Close() }()
	sid := str(req, "session")
	topK := intField(req, "top_k", 6)
	ans := c.AnswerOpts(ctx, hosted.AnswerOptions{Question: q, TopK: topK, SessionID: sid})
	return okResp(string(VerbMemoryAsk), map[string]any{"answer": ans})
}
