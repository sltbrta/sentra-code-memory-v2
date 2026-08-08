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
	"index":       "code_index",
	"code-index":  "code_index",
	"search":      "code_search",
	"code-search": "code_search",
	"relevant":    "code_find_relevant",
	"expand":      "code_expand",
	"impact":      "code_impact",
	"route":       "code_find_route",
	"freshness":   "code_freshness",
	"ingest":      "code_ingest_paths",
	"exact":       "code_exact",
	"defs":        "code_defs",
	"refs":        "code_refs",
	"memory-ask":  "memory_ask",
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
	kind := fs.String("kind", "any", "exact kind: any|definition|reference|import")
	topK := fs.Int("top-k", 20, "maximum hits")
	workers := fs.Int("workers", 4, "index worker count")
	force := fs.Bool("force", false, "force a full refresh")
	noRefresh := fs.Bool("no-refresh", false, "use an existing durable index")
	seed := fs.String("seed", "", "symbol seed for expansion/impact")
	from := fs.String("from", "", "route source symbol")
	to := fs.String("to", "", "route destination symbol")
	paths := fs.String("paths", "", "comma-separated relative paths")
	maxDepth := fs.Int("max-depth", 3, "impact traversal depth")
	maxFiles := fs.Int("max-files", 64, "impact file limit")
	maxBridges := fs.Int("max-bridges", 12, "route bridge limit")
	preview := fs.Bool("preview", true, "include source previews")
	dir := fs.String("dir", "", "local memory directory")
	session := fs.String("session", "", "memory session id")
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
	put("dir", *dir)
	put("session", *session)
	req["top_k"] = *topK
	req["workers"] = *workers
	req["force"] = *force
	req["no_refresh"] = *noRefresh
	req["max_depth"] = *maxDepth
	req["max_files"] = *maxFiles
	req["max_bridges"] = *maxBridges
	req["preview"] = *preview
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
  index, search, relevant, exact, defs, refs
  expand, impact, route, freshness, ingest, memory-ask
  catalog, ping, serve

Common flags:
  --root PATH --index-cache DIR --q QUERY --top-k N --workers N
  --no-refresh --force

The JSONL protocol is discoverable with catalog and returns one JSON response
per request. Use --help on this command for the complete contract.`)
}
