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
	case VerbCodeRead:
		return handleCodeRead(req)
	case VerbCodeImports:
		return handleCodeImports(ctx, req)
	case VerbCodeWatch:
		return handleCodeWatch(ctx, req)
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
	t0 := time.Now()
	payload := idx.FindRelevant(rootAbs, q, topK, preview)
	out := map[string]any{
		"payload": payload, "duration_ms": time.Since(t0).Milliseconds(),
		"search_backend": "codecrawl",
	}
	if bounded {
		out["context"] = packFindRelevant(req, rootAbs, payload, mode, maxBytes, maxTokens)
	}
	return okResp(string(VerbFindRelevant), out)
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

// --- code_read: bounded source-region read with path safety -----------------

const (
	defaultStartLine = 1
	defaultMaxLines  = 200
	maxLinesCap      = 1_000
)

// handleCodeRead reads a bounded source region from an indexed workspace.
// Path traversal, absolute paths, and symlink escapes are rejected.
// start_line defaults to 1; max_lines defaults to 200 and is capped at 1000.
func handleCodeRead(req Request) Response {
	path := str(req, "path")
	if path == "" {
		return errResp(string(VerbCodeRead), "path required")
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
	if fi.IsDir() {
		return codeErrResp(string(VerbCodeRead), ErrInvalidRequest,
			"path is a directory")
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
	line := 0
	endLine := startLine
	truncated := false
	for scanner.Scan() {
		line++
		if line < startLine {
			continue
		}
		if line-startLine+1 > maxLines {
			truncated = true
			break
		}
		if line > startLine {
			buf.WriteByte('\n')
		}
		buf.WriteString(scanner.Text())
		endLine = line
	}
	if err := scanner.Err(); err != nil && !truncated {
		return codeErrResp(string(VerbCodeRead), ErrInternal, err.Error())
	}
	// Check for truncation: more content available beyond the read window.
	if !truncated && scanner.Scan() {
		truncated = true
	}
	return okResp(string(VerbCodeRead), map[string]any{
		"path":       path,
		"content":    buf.String(),
		"start_line": startLine,
		"end_line":   endLine,
		"truncated":  truncated,
	})
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

	var events []WatchEvent
	opt := codecrawl.WatchOptions{
		Root:    rootAbs,
		GobPath: gobPath,
		Workers: workers,
		OnRefresh: func(st codecrawl.Stats, wrote bool) {
			events = append(events, WatchEvent{
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
			events = append(events, WatchEvent{
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
		d := time.Duration(milliseconds) * time.Millisecond
		if d < 0 || d > maxWatchDuration {
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
	var watchErr error
	if fsnotify {
		watchErr = codecrawl.WatchFS(ctx, opt)
	} else {
		watchErr = codecrawl.WatchPoll(ctx, opt)
	}
	if watchErr != nil && watchErr != context.Canceled {
		return codeErrResp(string(VerbCodeWatch), ErrInternal, watchErr.Error())
	}
	if watchErr == context.Canceled && ctx.Err() != nil {
		return codeErrResp(string(VerbCodeWatch), ErrInternal, watchErr.Error())
	}
	// The events slice captures refresh cycles and callback-level errors.
	if events == nil {
		events = []WatchEvent{}
	}
	return okResp(string(VerbCodeWatch), map[string]any{
		"root":        rootAbs,
		"gob_path":    gobPath,
		"duration_ms": time.Since(t0).Milliseconds(),
		"events":      events,
	})
}
