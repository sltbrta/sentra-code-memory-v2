// product-brain platform: tenant, federation, authority, serve.
package main

import (
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
	_ = fs.Parse(args)
	dec := json.NewDecoder(os.Stdin)
	enc := json.NewEncoder(os.Stdout)
	ctx := context.Background()
	for {
		var req map[string]any
		if err := dec.Decode(&req); err != nil {
			if err == io.EOF {
				return
			}
			_ = enc.Encode(map[string]any{"ok": false, "error": err.Error()})
			continue
		}
		_ = enc.Encode(codeserve.Handle(ctx, codeserve.Request(req)))
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
