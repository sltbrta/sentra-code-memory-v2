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
	"exact":                "code_exact",
	"defs":                 "code_defs",
	"refs":                 "code_refs",
	"read":                 "code_read",
	"imports":              "code_imports",
	"memory-ask":           "memory_ask",
	"memory-put":           "memory_put",
	"memory-search":        "memory_search",
	"memory-list":          "memory_list",
	"memory-promote":       "memory_promote",
	"session-continuation": "session_continuation",
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
	switch args[0] {
	case "catalog":
		return writeJSON(out, codeserve.Handle(context.Background(), codeserve.Request{"verb": "catalog"}))
	case "ping":
		return writeJSON(out, codeserve.Handle(context.Background(), codeserve.Request{"verb": "ping"}))
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
		return writeJSON(out, codeserve.Handle(context.Background(), req))
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
	kind := fs.String("kind", "", "kind: exact any|definition|reference|import, or memory note|preference|task|fact")
	topK := fs.Int("top-k", 20, "maximum hits")
	workers := fs.Int("workers", 4, "index worker count")
	force := fs.Bool("force", false, "force a full refresh")
	noRefresh := fs.Bool("no-refresh", false, "use an existing durable index")
	seed := fs.String("seed", "", "symbol seed for expansion/impact")
	from := fs.String("from", "", "route source symbol")
	to := fs.String("to", "", "route destination symbol")
	paths := fs.String("paths", "", "comma-separated relative paths")
	readPath := fs.String("path", "", "workspace-relative source path")
	startLine := fs.Int("start-line", 1, "first source line for code_read")
	maxLines := fs.Int("max-lines", 200, "maximum source lines for code_read")
	maxDepth := fs.Int("max-depth", 3, "impact traversal depth")
	maxFiles := fs.Int("max-files", 64, "impact file limit")
	maxBridges := fs.Int("max-bridges", 12, "route bridge limit")
	preview := fs.Bool("preview", true, "include source previews")
	dir := fs.String("dir", "", "local memory/session/savings directory")
	session := fs.String("session", "", "memory session id")
	// Typed memory / session / savings operators (issue #47).
	principal := fs.String("principal", "", "agent-memory principal (policy gate)")
	text := fs.String("text", "", "agent-memory entry text")
	tier := fs.String("tier", "", "agent-memory tier (stm|mtm|ltm)")
	tags := fs.String("tags", "", "comma-separated agent-memory tags")
	memID := fs.String("id", "", "agent-memory entry id")
	limit := fs.Int("limit", 50, "agent-memory result limit")
	repo := fs.String("repository", "", "session continuation base repository")
	tree := fs.String("tree", "", "session continuation base tree/ref")
	now := fs.String("now", "", "session continuation RFC3339 build time (deterministic replays)")
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
	put("dir", *dir)
	put("session", *session)
	put("principal", *principal)
	put("text", *text)
	put("kind", *kind)
	put("tier", *tier)
	put("tags", *tags)
	put("id", *memID)
	put("repository", *repo)
	put("tree", *tree)
	put("now", *now)
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
	req["limit"] = *limit
	return req, 0
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
  expand, impact, route, freshness, ingest, memory-ask
  memory-put, memory-search, memory-list, memory-promote
  session-continuation, savings-summary
  catalog, ping, serve, mlx
  http, mcp  # local HTTP and MCP-stdio adapters (issue #35)

Common flags:
  --root PATH --index-cache DIR --q QUERY --top-k N --workers N
  --no-refresh --force

Memory/session/savings flags (issue #47):
  --dir DIR --principal P --text T --kind K --tier stm|mtm|ltm
  --tags a,b --id ID --limit N --repository R --tree T --now RFC3339

The JSONL protocol is discoverable with catalog and returns one JSON response
per request. Use watch for debounced multi-worker freshness with retries, and
mlx start|stop|status|env for fully offline local inference. Use --help on this
command for the complete contract.`)
}
