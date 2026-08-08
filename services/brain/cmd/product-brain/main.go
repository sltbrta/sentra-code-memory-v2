// product-brain: product-owned create / ingest / ask, hosted write,
// and code-index / code-search (codecrawl multi-crawler).
package main

import (
	"fmt"
	"os"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	cmd := os.Args[1]
	// help is free — no timer noise
	if cmd == "help" || cmd == "-h" || cmd == "--help" {
		usage()
		return
	}
	// Wall-clock timer for every action (stderr JSON + human line).
	// Disable: OUROBOROS_CLI_TIMING=0
	timer := startAction(cmd)
	defer timer.Finish()

	switch cmd {
	case "create":
		runCreate(os.Args[2:])
	case "create-hosted":
		runCreateHosted(os.Args[2:])
	case "hosted-burst":
		runHostedBurst(os.Args[2:])
	case "ingest":
		runIngest(os.Args[2:])
	case "ask":
		runAsk(os.Args[2:])
	case "memory":
		runMemory(os.Args[2:])
	case "tenant":
		runTenant(os.Args[2:])
	case "federated-ask":
		runFederatedAsk(os.Args[2:])
	case "code-index":
		runCodeIndex(os.Args[2:])
	case "code-search":
		runCodeSearch(os.Args[2:])
	case "code-find-relevant":
		runCodeFindRelevant(os.Args[2:])
	case "code-expand":
		runCodeExpand(os.Args[2:])
	case "code-impact":
		runCodeImpact(os.Args[2:])
	case "code-find-route":
		runCodeFindRoute(os.Args[2:])
	case "code-defs":
		runCodeDefsRefs(os.Args[2:], true)
	case "code-refs":
		runCodeDefsRefs(os.Args[2:], false)
	case "serve":
		runServe(os.Args[2:])
	case "code-freshness":
		runCodeFreshness(os.Args[2:])
	case "code-ingest-paths":
		runCodeIngestPaths(os.Args[2:])
	case "code-watch":
		runCodeWatch(os.Args[2:])
	case "watch", "watch-docs":
		runWatchDocs(os.Args[2:])
	case "gardener":
		runGardener(os.Args[2:])
	case "project-hotlex":
		runProjectHotlex(os.Args[2:])
	case "search":
		runUnifiedSearch(os.Args[2:], false)
	case "search-ask":
		runUnifiedSearch(os.Args[2:], true)
	case "code-exact":
		runCodeExact(os.Args[2:])
	case "authority":
		runAuthority(os.Args[2:])
	case "mlx":
		runMLX(os.Args[2:])
	case "tui":
		runTUI(os.Args[2:])
	case "dense-bakeoff":
		runDenseBakeoff(os.Args[2:])
	case "planes":
		runPlanes(os.Args[2:])
	default:
		fatal("unknown command " + cmd)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `usage: product-brain <command> ...

ONE product binary (ADR 0022/0023): company brain + code operator + authority substrate.

company brain: create|ingest|ask|gardener|memory; code-* is workspace operator; authority is ACL/Git substrate

commands:
  create|ingest|ask|watch|gardener     company-doc residual (hosted + memory cortex + gardener)
  watch --dir DIR --docs PATH[,PATH…]  continual ingest (foreground or daemon)
  watch --dir DIR --registry FILE      multi-folder registry (TUI / launchd)
  ask --profile single_user|multi_principal --principal P
  gardener --once|--lifecycle [--rem] [--lifecycle-interval 30m]
  mlx start|stop|status|env            local MLX OpenAI-compatible lifecycle (BYOC)
  tui                                  single product pane TUI (Brain/Ops/Work/System)
  dense-bakeoff [--sizes 256,2048,8192] [--top-k 10] [--out receipt.json]
                                       exact-vs-ANN recall/latency/resource receipt
  planes                               dual-plane honesty inventory (GAP-PLANE-*)
  tenant create|status|list|disable|brain-create
  federated-ask --q --principal --cards path:id
  memory claim-admit|claim-list|episode-*|put|get|search|utility
  code-index|code-search|code-watch|…  codecrawl (workspace operator)
  code-exact --root --q [--kind]       P5 exact via codeindex
  search|search-ask --profile code|code_exact|local|hosted|auto
  serve  (JSONL multi-verb: catalog|code_*|memory_ask) | project-hotlex | hosted-burst
  authority --bootstrap PATH --bootstrap-sha256 HEX

ONE residual pipeline (ADR 0024): modules use pluggable substrates, not two products.
  Hosted preferred when Neon+Qdrant/keys configured; solo/FS is offline fallback.
  create|ingest|ask|gardener share OpenResidual: --chunks/--backend fs|memory|neon
    --queue sqlite|memory|ephemeral|none  --cortex fs|memory|none
    --substrate-profile solo|team|bench
  Env: OUROBOROS_BRAIN_PROFILE, OUROBOROS_BRAIN_DIR, OUROBOROS_BRAIN_QUEUE, OUROBOROS_BRAIN_CORTEX,
       OUROBOROS_BRAIN_CHUNKS, OUROBOROS_BRAIN_DENSE=none|qdrant|sqlite|postgres|faiss|memory
       OUROBOROS_BRAIN_QUEUE_DSN / DENSE_DSN / DENSE_URL (postgres / faiss BYOC)
	   OUROBOROS_BRAIN_DENSE_SEARCH_MODE=auto|exact|ann (local HNSW override)
       OUROBOROS_BRAIN_WORKERS=N local burst+gardener fleet (default GOMAXPROCS)
       OUROBOROS_BRAIN_LLM|EMBED|RANKER=hosted|mlx|none
       OUROBOROS_BRAIN_MLX_BASE_URL (default http://127.0.0.1:8080/v1) BYOC
  queue=sqlite default; queue=postgres for parallel local R/W; memory+dir→sqlite
  Local workers replace hosted burst when residual is local.
  OUROBOROS_BRAIN_GARDENER_AUTO=1 background enrich+cortex after open
Local dir: meta.json, chunks.jsonl, dense.db, memory/, security.json, sessions/, gardener.db
Timing: every command prints stderr cli_timing JSON + "timing  <cmd>  <ms>ms"
  Disable: OUROBOROS_CLI_TIMING=0
Docs: docs/decisions/0024-one-pipeline-substrate-modules.md`)
}

func fatal(msg string) {
	fmt.Fprintln(os.Stderr, msg)
	os.Exit(2)
}
