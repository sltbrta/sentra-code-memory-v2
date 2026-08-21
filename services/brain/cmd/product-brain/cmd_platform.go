// product-brain platform: tenant, federation, authority, serve.
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/codeserve"
	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/federation"
	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/hosted"
	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/tenant"
	"github.com/sltbrta/sentra-code-memory-v2/services/gateway/authorityprocess"
)

func runAuthority(args []string) {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, `usage: product-brain authority --bootstrap <abs-path> --bootstrap-sha256 <hex64>

ONE product binary (ADR 0022): company-doc CLI + authority Unix-socket process.

  product-brain authority --bootstrap /path/bootstrap.json --bootstrap-sha256 <hex>


Company-doc / continual / gardener / codecrawl:
  product-brain create|ingest|watch|gardener|ask|code-*|code-exact

Env for async gardener on authority publish:
  OUROBOROS_BRAIN_GARDENER_DB=/path/to/gardener.db
  product-brain gardener --dir <brain>   # drain queue

Comparison: docs/specs/product/STAGE-VS-PRODUCT.md`)
		os.Exit(2)
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err := authorityprocess.Run(ctx, args); err != nil {
		fmt.Fprintln(os.Stderr, "product-brain authority: startup rejected")
		os.Exit(1)
	}
}

// runServe is the multi-verb JSON line protocol (MCP-lite) for product brain.
// Each stdin line is {"verb":"...", ...args}; one JSON object per stdout line.
// Verb catalog: codeserve.Catalog() (Phase 1 SCM operator parity).
func runServe(args []string) {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	root := fs.String("root", "",
		"confine every request to this subtree (default: the working directory). "+
			"Pass --root=/ to serve the whole filesystem.")
	_ = fs.Parse(args)

	// This surface took no --root at all while the sibling binary's serve,
	// http and mcp were pinned. It is the same codeserve.Handle over the same
	// JSONL contract, so a request naming "/" was answered here after being
	// refused there: the pin reached three of the four surfaces that need it.
	pin, err := codeserve.ResolveRootFlag(*root)
	if err != nil {
		fmt.Fprintf(os.Stderr, "serve: %v\n", err)
		os.Exit(2)
	}
	ctx := codeserve.WithRootPin(context.Background(), pin)
	serveJSONL(ctx, os.Stdin, os.Stdout)
}

// serveJSONLMaxLine bounds one request line. It matches the sibling
// sentra-code-memory binary so the two JSONL surfaces agree.
const serveJSONLMaxLine = 8 << 20

// serveJSONL reads one JSON object per line and writes one response per line.
//
// It used to drive a json.Decoder in a loop and `continue` on error. A
// json.Decoder latches a syntax error and returns it from every subsequent
// Decode, so a single malformed byte turned this into an infinite hot loop
// writing an error line per iteration: measured at 3.7 million lines in three
// seconds, one core pegged, until the disk or the pipe consumer gave out.
//
// Line framing is what the documented contract already said ("one JSON object
// per stdin line"), and it resynchronises naturally: a bad line produces one
// error and the next line is parsed independently.
func serveJSONL(ctx context.Context, in io.Reader, out io.Writer) {
	scanner := bufio.NewScanner(in)
	scanner.Buffer(make([]byte, 64<<10), serveJSONLMaxLine)
	enc := json.NewEncoder(out)
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		var req map[string]any
		if err := json.Unmarshal(line, &req); err != nil {
			_ = enc.Encode(map[string]any{
				"ok": false, "error": "invalid JSON: " + err.Error(),
				"error_code": "invalid_request",
			})
			continue
		}
		_ = enc.Encode(codeserve.Handle(ctx, codeserve.Request(req)))
	}
	if err := scanner.Err(); err != nil {
		// A line past the bound cannot be answered, but the caller is owed a
		// framed response rather than silence.
		_ = enc.Encode(map[string]any{
			"ok": false, "error": "read request: " + err.Error(),
			"error_code": "invalid_request",
		})
	}
}

func runTenant(args []string) {
	if len(args) < 1 {
		fatal("tenant: create|status|list|disable|brain-create required")
	}
	fsRoot := flag.NewFlagSet("tenant", flag.ExitOnError)
	// Subcommand is args[0]; flags after.
	cmd := args[0]
	root := fsRoot.String("root", "", "tenant registry root (default: $OUROBOROS_TENANT_ROOT or .ouroboros-tenants)")
	id := fsRoot.String("id", "", "tenant id")
	region := fsRoot.String("region", "", "region/residency")
	brain := fsRoot.String("brain-id", "", "brain id for brain-create")
	_ = fsRoot.Parse(args[1:])
	rpath := *root
	if rpath == "" {
		rpath = os.Getenv("OUROBOROS_TENANT_ROOT")
	}
	if rpath == "" {
		rpath = ".ouroboros-tenants"
	}
	reg := &tenant.Registry{Root: rpath}
	switch cmd {
	case "create":
		if *id == "" {
			fatal("tenant create: --id required")
		}
		rec, err := reg.Create(*id, *region)
		if err != nil {
			fatal(err.Error())
		}
		emitJSON(map[string]any{"event": "tenant_create", "tenant": rec, "product_owned": true})
	case "status":
		if *id == "" {
			fatal("tenant status: --id required")
		}
		rec, err := reg.Status(*id)
		if err != nil {
			fatal(err.Error())
		}
		emitJSON(map[string]any{"event": "tenant_status", "tenant": rec, "product_owned": true})
	case "list":
		list, err := reg.List()
		if err != nil {
			fatal(err.Error())
		}
		emitJSON(map[string]any{"event": "tenant_list", "tenants": list, "product_owned": true})
	case "disable":
		if *id == "" {
			fatal("tenant disable: --id required")
		}
		if err := reg.Disable(*id); err != nil {
			fatal(err.Error())
		}
		emitJSON(map[string]any{"event": "tenant_disable", "id": *id, "product_owned": true})
	case "brain-create":
		if *id == "" || *brain == "" {
			fatal("tenant brain-create: --id and --brain-id required")
		}
		if _, err := reg.Status(*id); err != nil {
			fatal(err.Error())
		}
		bdir := reg.BrainDir(*id, *brain)
		if err := reg.AuthorizeBrainPath(*id, bdir); err != nil {
			fatal(err.Error())
		}
		c, err := hosted.CreateLocal(bdir, *brain)
		if err != nil {
			// open if exists
			c, err = hosted.OpenLocal(bdir, *brain)
			if err != nil {
				fatal(err.Error())
			}
		}
		_ = c.Close()
		emitJSON(map[string]any{
			"event": "tenant_brain_create", "tenant": *id, "brain_id": *brain,
			"dir": bdir, "product_owned": true,
		})
	default:
		fatal("tenant: unknown subcommand " + cmd)
	}
}

func runFederatedAsk(args []string) {
	fs := flag.NewFlagSet("federated-ask", flag.ExitOnError)
	q := fs.String("q", "", "question")
	principal := fs.String("principal", "", "principal")
	// cards: path=brainId pairs comma-separated path:id
	cardsFlag := fs.String("cards", "", "comma-separated path:brain_id[:principal_allow]")
	topK := fs.Int("top-k", 6, "top k")
	_ = fs.Parse(args)
	if *q == "" || *principal == "" || *cardsFlag == "" {
		fatal("federated-ask: --q --principal --cards required")
	}
	var cards []federation.BrainCard
	for _, part := range strings.Split(*cardsFlag, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		bits := strings.Split(part, ":")
		if len(bits) < 2 {
			fatal("federated-ask: card format path:brain_id")
		}
		c := federation.BrainCard{Path: bits[0], BrainID: bits[1], DocCount: 1}
		if len(bits) > 2 {
			c.AllowedFor = strings.Split(bits[2], "+")
		} else {
			c.AllowedFor = []string{*principal}
		}
		cards = append(cards, c)
	}
	res := federation.Ask(context.Background(), federation.AskOpts{
		Principal: *principal, Query: *q, TopK: *topK, MaxBrains: 4, Cards: cards,
	})
	emitJSON(map[string]any{"event": "federated_ask", "result": res, "product_owned": true})
}
