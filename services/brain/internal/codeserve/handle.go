package codeserve

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/codecrawl"
	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/contextpack"
	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/hosted"
	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/memory"
	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/productsearch"
	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/repoignore"
	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/savings"
	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/sessionlog"
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
	case VerbCodeRead:
		return handleCodeRead(ctx, req)
	case VerbCodeImports:
		return handleCodeImports(ctx, req)
	case VerbCodeWatch:
		return handleCodeWatch(ctx, req)
	case VerbRepoMap:
		return handleRepoMap(req)
	case VerbStructuralSearch:
		return handleStructuralSearch(req)
	case VerbDiagnostics:
		return handleDiagnostics(req)
	case VerbApplyChangeSet:
		return handleApplyChangeSet(ctx, req)
	case VerbMemoryAsk:
		return handleMemoryAsk(ctx, req)
	case VerbMemoryPut:
		return handleMemoryPut(req)
	case VerbMemorySearch:
		return handleMemorySearch(req)
	case VerbMemoryList:
		return handleMemoryList(req)
	case VerbMemoryPromote:
		return handleMemoryPromote(req)
	case VerbSessionContinuation:
		return handleSessionContinuation(req)
	case VerbSavingsSummary:
		return handleSavingsSummary(req)
	case VerbLifecycleInstall, VerbSessionProduct, VerbCodeDenseRerank, VerbHostedTenancy, VerbQueryAdvanced:
		return deferredResp(Verb(verb))
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
	gobPath = codecrawl.DefaultIndexPath(rootAbs)
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
	if workers > 256 {
		workers = 256
	}
	noRefresh := boolField(req, "no_refresh", false)
	force := boolField(req, "force", false)
	if noRefresh {
		idx, meta, err := codecrawl.Load(gobPath)
		if err != nil {
			return nil, "", "", err
		}
		if rootAbs != "" {
			// no_refresh must fail clearly on root mismatch rather than
			// serve another workspace's index.
			if err := codecrawl.ValidateRoot(meta, rootAbs); err != nil {
				return nil, "", "", err
			}
		} else {
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

// findRelevantSourceCap bounds per-file bytes read for packing (fail-safe).
const findRelevantSourceCap = 1 << 20

// findRelevantCandidateCap bounds packing candidates per request.
const findRelevantCandidateCap = 128
const findRelevantSessionCap = 256

// findRelevantSessions holds per-session dedup/handle registries for bounded
// code_find_relevant calls. In-process only; never persisted.
var findRelevantSessions = struct {
	sync.Mutex
	regs  map[string]*contextpack.Registry
	order []string
}{regs: map[string]*contextpack.Registry{}}

func sessionRegistry(id string) *contextpack.Registry {
	findRelevantSessions.Lock()
	defer findRelevantSessions.Unlock()
	reg, ok := findRelevantSessions.regs[id]
	if !ok {
		reg = contextpack.NewRegistry()
		findRelevantSessions.regs[id] = reg
		findRelevantSessions.order = append(findRelevantSessions.order, id)
		if len(findRelevantSessions.order) > findRelevantSessionCap {
			old := findRelevantSessions.order[0]
			findRelevantSessions.order = findRelevantSessions.order[1:]
			delete(findRelevantSessions.regs, old)
		}
	}
	return reg
}

// packFindRelevant builds the bounded contextpack result for a payload. The
// legacy payload above is untouched; this only adds the packed view.
func packFindRelevant(req Request, rootAbs string, payload codecrawl.AgentPayload, mode contextpack.RenderMode, maxBytes, maxTokens int) contextpack.Result {
	budget := contextpack.Budget{MaxBytes: maxBytes, MaxTokens: maxTokens}
	gov := contextpack.NewGovernor(contextpack.Limits{
		MaxCandidates:  findRelevantCandidateCap,
		MaxOutputBytes: budget.ByteLimit(),
	}, nil)
	reg := contextpack.NewRegistry()
	if sid := str(req, "session"); sid != "" {
		reg = sessionRegistry(rootAbs + "\x00" + sid)
	}
	sources := make([]contextpack.Source, 0, len(payload.Hits))
	for _, h := range payload.Hits {
		if len(sources) >= findRelevantCandidateCap {
			break
		}
		abs, ok := codecrawl.SafeRootPath(rootAbs, h.Path)
		if !ok {
			continue
		}
		raw, err := os.ReadFile(abs)
		if err != nil {
			continue
		}
		if len(raw) > findRelevantSourceCap {
			raw = raw[:findRelevantSourceCap]
		}
		content := string(raw)
		sources = append(sources, contextpack.Source{
			Path: h.Path, Content: content, Score: h.Score,
			Direct:    h.Kind == "def",
			StartLine: 1, EndLine: strings.Count(content, "\n") + 1,
		})
	}
	return contextpack.Pack(contextpack.Request{
		Sources: sources, Budget: budget, Render: mode,
		Registry: reg, Governor: gov,
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
	if rootAbs != "" {
		// Freshness must fail clearly on root mismatch (issue #42).
		if err := codecrawl.ValidateRoot(meta, rootAbs); err != nil {
			return idxErrResp(string(VerbFreshness), err)
		}
	} else {
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
	} else if err := codecrawl.ValidateRoot(meta, rootAbs); err != nil {
		return idxErrResp(string(VerbIngestPaths), err)
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

func openMemoryStore(req Request) (*memory.Store, Response) {
	dir := str(req, "dir")
	if dir == "" {
		return nil, errResp(str(req, "verb"), "dir required")
	}
	store, err := memory.Open(dir)
	if err != nil {
		return nil, errResp(str(req, "verb"), err.Error())
	}
	return store, nil
}

func handleMemoryPut(req Request) Response {
	store, errRespIfAny := openMemoryStore(req)
	if store == nil {
		return errRespIfAny
	}
	principal := str(req, "principal")
	text := str(req, "text")
	if principal == "" || text == "" {
		return errResp(string(VerbMemoryPut), "principal and text required")
	}
	kind := str(req, "kind")
	tier := str(req, "tier")
	tags := strSliceField(req, "tags")
	entry, err := store.PutAgentMemoryTier(principal, kind, text, tags, tier)
	if err != nil {
		return errResp(string(VerbMemoryPut), err.Error())
	}
	return okResp(string(VerbMemoryPut), map[string]any{"entry": entry})
}

func handleMemorySearch(req Request) Response {
	store, errRespIfAny := openMemoryStore(req)
	if store == nil {
		return errRespIfAny
	}
	principal := str(req, "principal")
	if principal == "" {
		return errResp(string(VerbMemorySearch), "principal required")
	}
	q := str(req, "q")
	limit := intField(req, "limit", 50)
	entries := store.SearchAgentMemory(principal, q, limit)
	return okResp(string(VerbMemorySearch), map[string]any{
		"entries": entries, "count": len(entries),
	})
}

func handleMemoryList(req Request) Response {
	store, errRespIfAny := openMemoryStore(req)
	if store == nil {
		return errRespIfAny
	}
	principal := str(req, "principal")
	if principal == "" {
		return errResp(string(VerbMemoryList), "principal required")
	}
	limit := intField(req, "limit", 50)
	entries := store.GetAgentMemory(principal, limit)
	return okResp(string(VerbMemoryList), map[string]any{
		"entries": entries, "count": len(entries),
	})
}

func handleMemoryPromote(req Request) Response {
	store, errRespIfAny := openMemoryStore(req)
	if store == nil {
		return errRespIfAny
	}
	id := str(req, "id")
	tier := str(req, "tier")
	principal := str(req, "principal")
	if id == "" || tier == "" || principal == "" {
		return errResp(string(VerbMemoryPromote), "id, tier, and principal required")
	}
	if principal != "" {
		entries := store.SearchAgentMemory(principal, "", 10000)
		found := false
		for _, entry := range entries {
			if entry.ID == id {
				found = true
				break
			}
		}
		if !found {
			return codeErrResp(string(VerbMemoryPromote), ErrPathDenied, "memory entry is not owned by principal")
		}
	}
	if err := store.PromoteAgentMemory(id, tier); err != nil {
		return errResp(string(VerbMemoryPromote), err.Error())
	}
	return okResp(string(VerbMemoryPromote), map[string]any{"id": id, "tier": tier})
}

// --- Bounded session continuation composite (issue #47) -------------------

func handleSessionContinuation(req Request) Response {
	dir := str(req, "dir")
	if dir == "" {
		return errResp(string(VerbSessionContinuation), "dir required")
	}
	w, err := sessionlog.Open(dir)
	if err != nil {
		return errResp(string(VerbSessionContinuation), err.Error())
	}
	events := w.Events()
	base := sessionlog.Provenance{
		Repository: str(req, "repository"),
		Tree:       str(req, "tree"),
	}
	opts := sessionlog.DefaultContinuationOptions()
	opts.IncludeSuperseded = boolField(req, "include_superseded", false)
	budgets := sessionlog.Budgets{
		L0: intField(req, "l0_bytes", opts.Budgets.L0),
		L1: intField(req, "l1_bytes", opts.Budgets.L1),
		L2: intField(req, "l2_bytes", opts.Budgets.L2),
		L3: intField(req, "l3_bytes", opts.Budgets.L3),
	}
	opts.Budgets = budgets
	now := time.Now().UTC()
	if raw := str(req, "now"); raw != "" {
		parsed, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			return errResp(string(VerbSessionContinuation), "now must be RFC3339: "+err.Error())
		}
		now = parsed.UTC()
	}
	cont, err := sessionlog.BuildContinuation(events, base, opts, now)
	if err != nil {
		return errResp(string(VerbSessionContinuation), err.Error())
	}
	return okResp(string(VerbSessionContinuation), map[string]any{
		"continuation": cont, "source_events": len(events),
	})
}

// --- Local token-savings read (issue #47) ---------------------------------

func handleSavingsSummary(req Request) Response {
	dir := str(req, "dir")
	if dir == "" {
		return errResp(string(VerbSavingsSummary), "dir required")
	}
	ledger, err := savings.Open(dir)
	if err != nil {
		return errResp(string(VerbSavingsSummary), err.Error())
	}
	summary := ledger.Summary()
	return okResp(string(VerbSavingsSummary), map[string]any{
		"summary": summary, "text": summary.String(),
	})
}

// --- Deferred / non-goal disclosures (issue #47) --------------------------

// deferredSpec is the parity disclosure for one intentionally-retired verb.
type deferredSpec struct {
	Decision string // deferred | non_goal
	Reason   string
	Doc      string
}

// deferredSpecs is the canonical parity decision for every catalogued verb
// that the standalone product deliberately does not implement (issue #47).
// Keeping the disclosure next to the catalog means the gap is discoverable
// (catalog lists the verb) and honest (calling it returns a structured 501-
// class response instead of an opaque unknown-verb error).
var deferredSpecs = map[Verb]deferredSpec{
	VerbLifecycleInstall: {
		Decision: "deferred",
		Reason:   "managed-server install/service/hook/uninstall lifecycle is not owned by the standalone local CLI; see the parity decision doc",
		Doc:      "docs/decisions/0025-memory-session-lifecycle-parity.md",
	},
	VerbSessionProduct: {
		Decision: "deferred",
		Reason:   "full SCM session continuation product (agent continuation packets, latent development-state memory) is a different product class; the bounded local session_continuation composite is the standalone surface",
		Doc:      "docs/specs/product/SCM-SESSION-PRODUCT.md",
	},
	VerbCodeDenseRerank: {
		Decision: "deferred",
		Reason:   "dense/reranked retrieval lane is not wired into the standalone code operator; retrieval is the local heuristic lane only",
		Doc:      "docs/decisions/0025-memory-session-lifecycle-parity.md",
	},
	VerbHostedTenancy: {
		Decision: "non_goal",
		Reason:   "hosted multi-tenancy, cloud sync/overlays, and billing are explicit non-goals of the standalone local-first product",
		Doc:      "docs/roadmap/DEFERRED-AND-NON-GOALS.md",
	},
	VerbQueryAdvanced: {
		Decision: "deferred",
		Reason:   "prior query modes (patch plans, test hints, related graph/source context, greenfield/native-first) are not part of the standalone operator surface",
		Doc:      "docs/decisions/0025-memory-session-lifecycle-parity.md",
	},
}

// deferredResp returns the structured disclosure for a deferred verb. OK is
// false with a stable error_code=deferred so callers can distinguish a
// deliberate gap from a typo (unknown_verb) or a malformed request.
func deferredResp(verb Verb) Response {
	spec, ok := deferredSpecs[verb]
	if !ok {
		return codeErrResp(string(verb), ErrUnknownVerb, "unknown verb")
	}
	return Response{
		"ok": false, "verb": string(verb),
		"error":         spec.Decision + ": " + spec.Reason,
		"error_code":    string(ErrDeferred),
		"deferred":      true,
		"decision":      spec.Decision,
		"reason":        spec.Reason,
		"doc":           spec.Doc,
		"product_owned": true,
	}
}

// strSliceField reads a string-slice field that may arrive as a JSON array or
// a comma-separated string.
func strSliceField(req Request, key string) []string {
	switch v := req[key].(type) {
	case []any:
		out := make([]string, 0, len(v))
		for _, item := range v {
			if s, ok := item.(string); ok {
				if s = strings.TrimSpace(s); s != "" {
					out = append(out, s)
				}
			}
		}
		return out
	case string:
		var out []string
		for _, part := range strings.Split(v, ",") {
			if part = strings.TrimSpace(part); part != "" {
				out = append(out, part)
			}
		}
		return out
	default:
		return nil
	}
}

// --- code_read: bounded source-region read with path safety -----------------

const (
	defaultStartLine = 1
	defaultMaxLines  = 200
	maxLinesCap      = 1_000
	maxReadBytes     = 1 << 20
	maxReadLineBytes = 1 << 20
)

// handleCodeRead reads a bounded source region from an indexed workspace.
// Path traversal, absolute paths, and symlink escapes are rejected.
// start_line defaults to 1; max_lines defaults to 200 and is capped at 1000.
func handleCodeRead(ctx context.Context, req Request) Response {
	path := str(req, "path")
	if path == "" {
		return errResp(string(VerbCodeRead), "path required")
	}
	if err := ctx.Err(); err != nil {
		return codeErrResp(string(VerbCodeRead), ErrInternal, err.Error())
	}
	// Reject path traversal and absolute references.
	if strings.Contains(path, "..") || filepath.IsAbs(path) {
		return codeErrResp(string(VerbCodeRead), ErrInvalidRequest,
			"path must be workspace-relative")
	}
	root := str(req, "root")
	if root == "" {
		return errResp(string(VerbCodeRead), "root required")
	}
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return codeErrResp(string(VerbCodeRead), ErrInvalidRequest, err.Error())
	}
	// Resolve the root itself (e.g. /var on macOS → /private/var) so that
	// symlink containment checks work across filesystem boundaries.
	rootReal, err := filepath.EvalSymlinks(rootAbs)
	if err != nil {
		rootReal = rootAbs
	}
	resolved := filepath.Join(rootAbs, filepath.Clean(path))
	// Resolve symlinks and verify the target stays inside root.
	real, err := filepath.EvalSymlinks(resolved)
	if err != nil {
		return codeErrResp(string(VerbCodeRead), ErrInvalidRequest,
			"cannot resolve path: "+err.Error())
	}
	rel, err := filepath.Rel(rootReal, real)
	if err != nil || strings.HasPrefix(rel, "..") {
		return codeErrResp(string(VerbCodeRead), ErrInvalidRequest,
			"path escapes workspace root")
	}
	fi, err := os.Stat(real)
	if err != nil {
		return codeErrResp(string(VerbCodeRead), ErrIndexUnavailable, err.Error())
	}
	if !fi.Mode().IsRegular() {
		return codeErrResp(string(VerbCodeRead), ErrInvalidRequest,
			"path is not a regular file")
	}
	// Trust gates (issue #41): by default a read must also survive the
	// repository ignore policy and — when a durable index exists — index
	// membership. allow_ignored / allow_unindexed are the explicit opt-ins.
	if denial := gateCodeReadPath(req, rootAbs, rootReal, real, path); denial != nil {
		return denial
	}
	startLine := intField(req, "start_line", defaultStartLine)
	if startLine < 1 {
		startLine = 1
	}
	maxLines := intField(req, "max_lines", defaultMaxLines)
	if maxLines < 1 {
		maxLines = defaultMaxLines
	}
	if maxLines > maxLinesCap {
		maxLines = maxLinesCap
	}

	f, err := os.Open(real)
	if err != nil {
		return codeErrResp(string(VerbCodeRead), ErrInternal, err.Error())
	}
	defer f.Close()

	var buf strings.Builder
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64<<10), maxReadLineBytes)
	line := 0
	endLine := startLine - 1
	truncated := false
	for scanner.Scan() {
		if err := ctx.Err(); err != nil {
			return codeErrResp(string(VerbCodeRead), ErrInternal, err.Error())
		}
		line++
		if line < startLine {
			continue
		}
		if line-startLine+1 > maxLines {
			truncated = true
			break
		}
		text := scanner.Text()
		additional := len(text)
		if endLine >= startLine {
			additional++
		}
		if buf.Len()+additional > maxReadBytes {
			truncated = true
			break
		}
		if endLine >= startLine {
			buf.WriteByte('\n')
		}
		buf.WriteString(text)
		endLine = line
	}
	if err := scanner.Err(); err != nil && !truncated {
		return codeErrResp(string(VerbCodeRead), ErrInternal, err.Error())
	}
	return okResp(string(VerbCodeRead), map[string]any{
		"path":       path,
		"content":    buf.String(),
		"start_line": startLine,
		"end_line":   endLine,
		"truncated":  truncated,
	})
}

// gateCodeReadPath enforces the repoignore and index-membership read policy
// (issue #41). It returns nil when the read may proceed, else a structured
// denial response. Path/symlink/regular-file checks run before this gate and
// are never bypassed by the opt-in fields.
func gateCodeReadPath(req Request, rootAbs, rootReal, real, reqPath string) Response {
	verb := string(VerbCodeRead)
	if !boolField(req, "allow_ignored", false) {
		matcher, err := repoignore.Load(rootReal)
		if err != nil {
			// Fail closed: an unreadable ignore file must not broaden reads.
			return codeErrResp(verb, ErrInternal,
				"ignore policy unavailable: "+err.Error())
		}
		if rel, err := filepath.Rel(rootReal, real); err == nil && matcher.Ignored(rel, false) {
			return codeErrResp(verb, ErrPathDenied,
				"path denied by repository ignore policy (explicit opt-in: allow_ignored)")
		}
	}
	if boolField(req, "allow_unindexed", false) {
		return nil
	}
	gobPath := codecrawl.DefaultIndexPath(rootAbs)
	if cache := firstStr(req, "index_cache", "index-cache"); cache != "" {
		if out, err := filepath.Abs(cache); err == nil {
			gobPath = filepath.Join(out, "code-index.gob")
		}
	}
	if _, err := os.Stat(gobPath); err != nil {
		// Compatibility fallback: without a durable index the ignore gate is
		// the only membership policy available for this workspace.
		if os.IsNotExist(err) {
			return nil
		}
		return codeErrResp(verb, ErrInternal, err.Error())
	}
	idx, meta, err := codecrawl.Load(gobPath)
	if err != nil {
		// Fail closed: membership cannot be verified against a broken index.
		return codeErrResp(verb, ErrIndexUnavailable,
			"index membership cannot be verified: "+err.Error())
	}
	if err := codecrawl.ValidateRoot(meta, rootAbs); err != nil {
		return codeErrResp(verb, ErrIndexUnavailable, err.Error())
	}
	for _, cand := range readMembershipCandidates(rootAbs, rootReal, real, reqPath) {
		if idx.HasFile(cand) {
			return nil
		}
	}
	return codeErrResp(verb, ErrPathDenied,
		"path is not a member of the durable code index (explicit opt-in: allow_unindexed)")
}

// readMembershipCandidates yields the relative-path spellings under which the
// index may have recorded the resolved file: the requested path, the
// symlink-resolved relative path, and the unresolved-root relative path when
// the root itself traverses a symlink (e.g. /var on macOS).
func readMembershipCandidates(rootAbs, rootReal, real, reqPath string) []string {
	seen := map[string]bool{}
	out := []string{}
	add := func(p string) {
		if p != "" && !strings.HasPrefix(p, "..") && !filepath.IsAbs(p) && !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	add(filepath.Clean(reqPath))
	if rel, err := filepath.Rel(rootReal, real); err == nil {
		add(rel)
	}
	if rootAbs != rootReal {
		if rel, err := filepath.Rel(rootAbs, real); err == nil {
			add(rel)
		}
	}
	return out
}

func firstStr(req Request, keys ...string) string {
	for _, k := range keys {
		if v := str(req, k); v != "" {
			return v
		}
	}
	return ""
}

// --- code_imports: exact import lane equivalent to code_exact kind=import -----

func handleCodeImports(ctx context.Context, req Request) Response {
	root := str(req, "root")
	q := str(req, "q")
	if root == "" || q == "" {
		return errResp(string(VerbCodeImports), "root and q required")
	}
	topK := intField(req, "top_k", 20)
	t0 := time.Now()
	res := productsearch.Search(ctx, productsearch.Request{
		Profile: productsearch.ProfileCodeExact, CodeRoot: root, Question: q,
		TopK: topK, ExactKind: string(ExactKindImport),
	})
	return okResp(string(VerbCodeImports), map[string]any{
		"result":         res,
		"duration_ms":    time.Since(t0).Milliseconds(),
		"search_backend": "codeindex",
	})
}

// --- code_watch: bounded JSONL adapter over codecrawl watchers ---------------

const (
	defaultWatchMaxCycles = 1
	maxWatchCyclesCap     = 10_000
	maxWatchWorkers       = 256
	maxWatchQueueSize     = 1 << 20
	maxWatchDuration      = 24 * time.Hour
	defaultWatchTimeout   = 30 * time.Second
	maxWatchEvents        = 1_024
)

func handleCodeWatch(ctx context.Context, req Request) Response {
	root := str(req, "root")
	if root == "" {
		return errResp(string(VerbCodeWatch), "root required")
	}
	cache := str(req, "index_cache")
	if cache == "" {
		cache = str(req, "index-cache")
	}
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return errResp(string(VerbCodeWatch), err.Error())
	}
	gobPath := cache
	if gobPath != "" {
		gobPath = filepath.Join(cache, "code-index.gob")
	} else {
		gobPath = codecrawl.DefaultIndexPath(rootAbs)
	}
	workers := intField(req, "workers", 4)
	if workers <= 0 {
		workers = 4
	}
	if workers > maxWatchWorkers {
		workers = maxWatchWorkers
	}
	intervalMS := intField(req, "interval_ms", 0)
	debounceMS := intField(req, "debounce_ms", 0)
	queueSize := intField(req, "queue_size", 0)
	retryInitialMS := intField(req, "retry_initial_ms", 0)
	retryMaxMS := intField(req, "retry_max_ms", 0)
	maxCycles := intField(req, "max_cycles", defaultWatchMaxCycles)
	if maxCycles <= 0 {
		maxCycles = defaultWatchMaxCycles
	}
	if maxCycles > maxWatchCyclesCap {
		maxCycles = maxWatchCyclesCap
	}
	if queueSize > maxWatchQueueSize {
		queueSize = maxWatchQueueSize
	}
	fsnotify := boolField(req, "fsnotify", false)
	timeout := defaultWatchTimeout
	if timeoutMS := intField(req, "timeout_ms", 0); timeoutMS > 0 {
		if timeoutMS > int(maxWatchDuration/time.Millisecond) {
			timeout = maxWatchDuration
		} else {
			timeout = time.Duration(timeoutMS) * time.Millisecond
		}
	}

	var events []WatchEvent
	eventsTruncated := false
	appendEvent := func(event WatchEvent) {
		if len(events) >= maxWatchEvents {
			eventsTruncated = true
			return
		}
		events = append(events, event)
	}
	opt := codecrawl.WatchOptions{
		Root:    rootAbs,
		GobPath: gobPath,
		Workers: workers,
		OnRefresh: func(st codecrawl.Stats, wrote bool) {
			appendEvent(WatchEvent{
				Event:          "refresh",
				Root:           rootAbs,
				GobPath:        gobPath,
				Changed:        st.Changed,
				Unchanged:      st.Unchanged,
				SkippedByStamp: st.SkippedByStamp,
				DurationMS:     st.Duration.Milliseconds(),
				Workers:        workers,
				Wrote:          wrote,
				QueueDepth:     st.QueueDepth,
				RetryCount:     st.RetryCount,
				FullRescan:     st.FullRescan,
			})
		},
		OnError: func(err error, retryCount int) {
			appendEvent(WatchEvent{
				Event:      "refresh_error",
				Root:       rootAbs,
				GobPath:    gobPath,
				Error:      err.Error(),
				RetryCount: retryCount,
			})
		},
		MaxCycles: maxCycles,
	}
	toDuration := func(milliseconds int) time.Duration {
		if milliseconds <= 0 {
			return 0
		}
		if milliseconds > int(maxWatchDuration/time.Millisecond) {
			return maxWatchDuration
		}
		d := time.Duration(milliseconds) * time.Millisecond
		if d <= 0 || d > maxWatchDuration {
			return maxWatchDuration
		}
		return d
	}
	if interval := toDuration(intervalMS); interval > 0 {
		opt.Interval = interval
	}
	if debounce := toDuration(debounceMS); debounce > 0 {
		opt.Debounce = debounce
	}
	if queueSize > 0 {
		opt.QueueSize = queueSize
	}
	if retryInitial := toDuration(retryInitialMS); retryInitial > 0 {
		opt.RetryInitial = retryInitial
	}
	if retryMax := toDuration(retryMaxMS); retryMax > 0 {
		opt.RetryMax = retryMax
	}

	t0 := time.Now()
	watchCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var watchErr error
	if fsnotify {
		watchErr = codecrawl.WatchFS(watchCtx, opt)
	} else {
		watchErr = codecrawl.WatchPoll(watchCtx, opt)
	}
	cancelled := watchCtx.Err() != nil
	if watchErr != nil && !cancelled {
		return codeErrResp(string(VerbCodeWatch), ErrIndexUnavailable, watchErr.Error())
	}
	// A bounded timeout or caller cancellation returns the useful partial
	// event stream rather than wedging JSONL or discarding prior work.
	if events == nil {
		events = []WatchEvent{}
	}
	return okResp(string(VerbCodeWatch), map[string]any{
		"root":             rootAbs,
		"gob_path":         gobPath,
		"duration_ms":      time.Since(t0).Milliseconds(),
		"events":           events,
		"cancelled":        cancelled,
		"events_truncated": eventsTruncated,
	})
}
