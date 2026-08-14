// Command sentra-code-memory is the small, agent-facing control plane for the
// standalone code-memory backend. It offers direct verbs and a JSONL protocol.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/codeserve"
)

const maxRequestBytes = 8 << 20

var aliases = map[string]string{
	"index":                "code_index",
	"code-index":           "code_index",
	"search":               "code_search",
	"code-search":          "code_search",
	"relevant":             "code_find_relevant",
	"expand":               "code_expand",
	"impact":               "code_impact",
	"route":                "code_find_route",
	"freshness":            "code_freshness",
	"ingest":               "code_ingest_paths",
	"ingest-scip":          "code_ingest_scip",
	"exact":                "code_exact",
	"defs":                 "code_defs",
	"refs":                 "code_refs",
	"read":                 "code_read",
	"imports":              "code_imports",
	"repo-map":             "code_repo_map",
	"structural":           "code_structural_search",
	"diagnostics":          "code_diagnostics",
	"apply-changeset":      "code_apply_changeset",
	"memory-ask":           "memory_ask",
	"memory-put":           "memory_put",
	"memory-search":        "memory_search",
	"memory-list":          "memory_list",
	"memory-promote":       "memory_promote",
	"session-continuation": "session_continuation",
	"session-recall":       "session_recall",
	"savings-summary":      "savings_summary",
}

func main() {
	os.Exit(execute(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

func execute(args []string, in io.Reader, out, errOut io.Writer) int {
	if len(args) == 0 || args[0] == "help" || args[0] == "--help" || args[0] == "-h" {
		writeHelp(out)
		return 0
	}
	// Single caller context for the process lifetime; subcommands thread it
	// through to codeserve so cancellation propagates instead of each verb
	// starting from context.Background().
	ctx := context.Background()
	switch args[0] {
	case "catalog":
		return writeJSON(out, codeserve.Handle(ctx, codeserve.Request{"verb": "catalog"}))
	case "ping":
		return writeJSON(out, codeserve.Handle(ctx, codeserve.Request{"verb": "ping"}))
	case "serve":
		return serve(args[1:], in, out, errOut)
	case "watch":
		return runWatch(args[1:], out, errOut)
	case "mlx":
		return runMLX(args[1:], out, errOut)
	case "http":
		return runHTTP(args[1:], out, errOut)
	case "mcp":
		return runMCP(args[1:], in, out, errOut)
	case "hooks":
		return runHooks(ctx, args[1:], out, errOut)
	case "dense-local":
		return runDenseLocal(ctx, args[1:], out, errOut)
	default:
		verb, ok := aliases[args[0]]
		if !ok {
			fmt.Fprintf(errOut, "unknown command %q; use catalog or --help\n", args[0])
			return 2
		}
		req, code := parseRequest(verb, args[1:], errOut)
		if code != 0 {
			return code
		}
		return writeJSON(out, codeserve.Handle(ctx, req))
	}
}

func serve(args []string, in io.Reader, out, errOut io.Writer) int {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.SetOutput(errOut)
	timeout := fs.Duration("timeout", 0, "per-request timeout (0 means caller lifetime)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(errOut, "serve does not accept positional arguments")
		return 2
	}

	scanner := bufio.NewScanner(in)
	scanner.Buffer(make([]byte, 64<<10), maxRequestBytes)
	line := 0
	for scanner.Scan() {
		line++
		var req codeserve.Request
		if err := json.Unmarshal(scanner.Bytes(), &req); err != nil {
			fmt.Fprintf(errOut, "line %d: invalid JSON: %v\n", line, err)
			return 2
		}
		if req == nil {
			fmt.Fprintf(errOut, "line %d: request must be a JSON object\n", line)
			return 2
		}
		ctx := context.Background()
		cancel := func() {}
		if *timeout > 0 {
			ctx, cancel = context.WithTimeout(ctx, *timeout)
		}
		response := codeserve.Handle(ctx, req)
		cancel()
		if code := writeJSON(out, response); code != 0 {
			return code
		}
	}
	if err := scanner.Err(); err != nil {
		fmt.Fprintf(errOut, "read JSONL: %v\n", err)
		return 2
	}
	return 0
}

func parseRequest(verb string, args []string, errOut io.Writer) (codeserve.Request, int) {
	fs := flag.NewFlagSet(verb, flag.ContinueOnError)
	fs.SetOutput(errOut)
	root := fs.String("root", "", "source root")
	cache := fs.String("index-cache", "", "index cache directory")
	query := fs.String("q", "", "query or symbol")
	kind := fs.String("kind", "any", "exact kind: any|definition|reference|import")
	topK := fs.Int("top-k", 20, "maximum hits")
	workers := fs.Int("workers", 4, "index worker count")
	force := fs.Bool("force", false, "force a full refresh")
	noRefresh := fs.Bool("no-refresh", false, "use an existing durable index")
	seed := fs.String("seed", "", "symbol seed for expansion/impact")
	from := fs.String("from", "", "route source symbol")
	to := fs.String("to", "", "route destination symbol")
	paths := fs.String("paths", "", "comma-separated relative paths")
	readPath := fs.String("path", "", "workspace-relative source path")
	language := fs.String("language", "", "source language for SCIP ingest")
	scipPath := fs.String("scip", "", "path to a SCIP JSON document")
	startLine := fs.Int("start-line", 1, "first source line for code_read")
	maxLines := fs.Int("max-lines", 200, "maximum source lines for code_read")
	maxDepth := fs.Int("max-depth", 3, "impact traversal depth")
	maxFiles := fs.Int("max-files", 64, "impact file limit")
	maxBridges := fs.Int("max-bridges", 12, "route bridge limit")
	preview := fs.Bool("preview", true, "include source previews")
	dir := fs.String("dir", "", "local memory directory")
	session := fs.String("session", "", "memory session id")
	mode := fs.String("mode", "", "request mode: fast|quality|deep")
	pattern := fs.String("pattern", "", "deterministic structural pattern")
	ruleID := fs.String("rule-id", "", "structural rule identifier")
	maxBytes := fs.Int("max-bytes", 0, "maximum returned context bytes")
	maxTokens := fs.Int("max-tokens", 0, "maximum estimated returned tokens")
	maxMatches := fs.Int("max-matches", 0, "maximum structural matches")
	changesetPath := fs.String("changeset", "", "path to ChangeSet JSON")
	principal := fs.String("principal", "", "agent-memory principal")
	text := fs.String("text", "", "agent-memory text")
	tier := fs.String("tier", "", "agent-memory tier")
	tags := fs.String("tags", "", "comma-separated memory tags")
	memoryID := fs.String("id", "", "agent-memory entry id")
	limit := fs.Int("limit", 50, "memory result limit")
	repository := fs.String("repository", "", "continuation repository")
	tree := fs.String("tree", "", "continuation tree")
	now := fs.String("now", "", "continuation RFC3339 time")
	includeSuperseded := fs.Bool("include-superseded", false, "include superseded continuation/recall events")
	minConfidence := fs.Float64("min-confidence", 0.5, "minimum session recall provenance confidence")
	minRelevance := fs.Float64("min-relevance", 0.35, "minimum session recall lexical relevance")
	l0Bytes := fs.Int("l0-bytes", 0, "continuation L0 budget")
	l1Bytes := fs.Int("l1-bytes", 0, "continuation L1 budget")
	l2Bytes := fs.Int("l2-bytes", 0, "continuation L2 budget")
	l3Bytes := fs.Int("l3-bytes", 0, "continuation L3 budget")
	allowIgnored := fs.Bool("allow-ignored", false, "allow code_read paths ignored by repository policy")
	allowUnindexed := fs.Bool("allow-unindexed", false, "allow code_read paths absent from durable index")
	render := fs.String("render", "", "context render mode: full|signatures|skeleton|compact")
	if err := fs.Parse(args); err != nil {
		return nil, 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintf(errOut, "%s does not accept positional arguments\n", verb)
		return nil, 2
	}

	req := codeserve.Request{"verb": verb}
	put := func(key, value string) {
		if strings.TrimSpace(value) != "" {
			req[key] = value
		}
	}
	put("root", *root)
	put("index_cache", *cache)
	put("q", *query)
	put("kind", *kind)
	put("seed", *seed)
	put("from", *from)
	put("to", *to)
	put("paths", *paths)
	put("path", *readPath)
	put("language", *language)
	put("dir", *dir)
	put("session", *session)
	put("principal", *principal)
	put("text", *text)
	put("tier", *tier)
	put("tags", *tags)
	put("id", *memoryID)
	put("repository", *repository)
	put("tree", *tree)
	put("now", *now)
	put("mode", *mode)
	put("render", *render)
	put("pattern", *pattern)
	if *includeSuperseded {
		req["include_superseded"] = true
	}
	if *l0Bytes > 0 {
		req["l0_bytes"] = *l0Bytes
	}
	if *l1Bytes > 0 {
		req["l1_bytes"] = *l1Bytes
	}
	if *l2Bytes > 0 {
		req["l2_bytes"] = *l2Bytes
	}
	if *l3Bytes > 0 {
		req["l3_bytes"] = *l3Bytes
	}
	put("rule_id", *ruleID)
	if *scipPath != "" {
		raw, err := readBoundedFile(*scipPath, maxRequestBytes)
		if err != nil {
			fmt.Fprintf(errOut, "read SCIP document: %v\n", err)
			return nil, 2
		}
		var document map[string]any
		if err := json.Unmarshal(raw, &document); err != nil || document == nil {
			fmt.Fprintf(errOut, "decode SCIP document: %v\n", err)
			return nil, 2
		}
		req["document"] = document
	}
	if *changesetPath != "" {
		raw, err := os.ReadFile(*changesetPath)
		if err != nil {
			fmt.Fprintf(errOut, "read changeset: %v\n", err)
			return nil, 2
		}
		var cs map[string]any
		if err := json.Unmarshal(raw, &cs); err != nil {
			fmt.Fprintf(errOut, "decode changeset: %v\n", err)
			return nil, 2
		}
		req["changeset"] = cs
	}
	req["top_k"] = *topK
	req["workers"] = *workers
	req["force"] = *force
	req["no_refresh"] = *noRefresh
	req["max_depth"] = *maxDepth
	req["max_files"] = *maxFiles
	req["max_bridges"] = *maxBridges
	req["start_line"] = *startLine
	req["max_lines"] = *maxLines
	req["preview"] = *preview
	req["allow_ignored"] = *allowIgnored
	req["allow_unindexed"] = *allowUnindexed
	req["limit"] = *limit
	req["min_confidence"] = *minConfidence
	req["min_relevance"] = *minRelevance
	if *maxBytes > 0 {
		req["max_bytes"] = *maxBytes
	}
	if *maxTokens > 0 {
		req["max_tokens"] = *maxTokens
	}
	if *maxMatches > 0 {
		req["max_matches"] = *maxMatches
	}
	return req, 0
}

func readBoundedFile(path string, limit int64) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	raw, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(raw)) > limit {
		return nil, fmt.Errorf("file exceeds %d bytes", limit)
	}
	return raw, nil
}

func writeJSON(out io.Writer, value any) int {
	encoder := json.NewEncoder(out)
	if err := encoder.Encode(value); err != nil {
		return 1
	}
	return 0
}

func writeHelp(out io.Writer) {
	fmt.Fprintln(out, `sentra-code-memory — standalone code memory for coding agents

Usage:
  sentra-code-memory catalog
  sentra-code-memory <command> [flags]
  sentra-code-memory serve [--timeout 2s]  # one JSON object per input line

Commands:
  index, search, relevant, exact, defs, refs, read, imports, watch
  expand, impact, route, freshness, ingest, ingest-scip, repo-map, structural, diagnostics
  apply-changeset, memory-ask
  memory-put, memory-search, memory-list, memory-promote
  session-continuation, session-recall, savings-summary
  catalog, ping, serve, mlx
  http, mcp  # local HTTP and MCP-stdio adapters (issue #35)
  hooks (install|status|uninstall|run)  # repo-confined git hook lifecycle (opt-in)
  dense-local                           # local lexical code retrieval (opt-in)

Common flags:
  --root PATH --index-cache DIR --q QUERY --top-k N --workers N
  --no-refresh --force

Hooks flags:
  --root PATH --strategy repo-hooks|git-common-hooks --kinds a,b
  --cli-path PATH --event NAME --allow-unsafe-git-common
  The action may appear before or after flags.

Dense-local flags (hard ceilings: top-k <= 50, corpus <= 8192 files,
dimension <= 1024, query <= 512 chars; flags may only tighten them):
  --root PATH --q TEXT --top-k N --scope NAME --model NAME
  --max-corpus N --max-dim N --max-query-len N

The JSONL protocol is discoverable with catalog and returns one JSON response
per request. Use watch for debounced multi-worker freshness with retries, and
mlx start|stop|status|env for fully offline local inference. Use --help on this
command for the complete contract.`)
}
