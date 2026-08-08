> **Historical snapshot** — see [REMAINING-GAPS.md](../roadmap/REMAINING-GAPS.md) / [memory README](../../services/brain/internal/memory/README.md) for current truth (2026-07-27 residual cortex).

# codecrawl vs Sentra Code Memory — functionality parity

**Date:** 2026-07-25  
**Donors:** SCM `/Users/sammy/Developer/Sentra_Research/sentra-code-memory`  
**Product:** `services/brain/internal/codecrawl` + `product-brain code-*`

## Verdict

Product **code search / index surface now maps to SCM core verbs** with
**heuristic** (not LSP/SCIP) authority, while keeping multi-crawler cold index
and **mtime-stamp warm refresh** latency advantages.

| Capability | SCM | Product now | Notes |
| --- | --- | --- | --- |
| Index local tree | yes | **yes** `code-index` | Parallel crawl |
| Durable index | CAS/Postgres | **yes** `code-index.gob` | File-local gob |
| File-incremental reindex | yes | **yes** stamp+hash delta | Warm skip content read |
| Freshness probe | yes | **yes** `code-freshness` | Stamp walk, no body read |
| Path-only ingest | `ingest_paths` | **yes** `code-ingest-paths` | |
| Watcher | notify | **yes** `code-watch` | Poll+debounce (no extra dep) |
| Lexical search | BM25+dense | **yes** TF+rank heuristics | Multi-lang |
| `find_relevant` lean payload | yes | **yes** `code-find-relevant` | AgentHit + preview |
| `expand` | yes | **yes** `code-expand` | Symbol/name hop |
| `impact` | yes | **yes** `code-impact` | Heuristic defs/refs/imports |
| `read` | yes | OS/agent | Not duplicated |
| `find_route` / bridges | yes | **gap** | SCM differentiator; deferred |
| Dense + ZE rerank (code) | optional | **gap** | Spend gate; company-doc has ZE |
| MCP multi-verb HTTP | yes | **partial** | `product-brain serve` JSONL via `codeserve` (Phase 1); full MCP stdio transport optional later |
| Compiler/LSP authority | partial | **gap** | Explicit in impact gaps |

## CLI map (SCM → product)

| SCM | product-brain |
| --- | --- |
| `scm index` | `code-index` |
| `scm find-relevant` | `code-find-relevant` |
| `scm expand` | `code-expand` |
| `scm impact` | `code-impact` |
| freshness | `code-freshness` |
| ingest_paths | `code-ingest-paths` |
| `scm watch` | `code-watch` |
| `scm find-route` | `code-find-route` / serve `code_find_route` |

## Refresh policy (latency-preserving)

1. **Stamps** (`mtime_ns` + `size`) stored per file in gob.  
2. Warm `OpenOrRefresh`: if **all stamps match** → return index with **0 bytes read**.  
3. Dirty files: content hash + re-tokenize only those; reuse subgraphs for the rest.  
4. `code-freshness`: stamp walk only (no token work).  
5. `code-watch`: poll freshness → debounced OpenOrRefresh.

## Ranking (VS Code)

| Version | hit@1 | hit@5 | hit@10 |
| --- | --- | --- | --- |
| Raw TF | 0% | 17% | 33% |
| log1p + demote + stem | 50% | 83% | 83% |
| + src boost + demote copilot mirrors | see latest `vscode-scm-parity-bench.json` | | |

Still weak: pure interface tokens without path stem; not jump-to-def.

## Honest non-claims

- Impact is **heuristic**, not call-graph / type-flow.  
- No route/bridge synthesizer (SCM differentiator — adopt only if product needs it).  
- No MCP server yet (CLI is the product operator surface).  
- No hosted SCM org/worktree overlays.

## Evidence

- `vscode-scm-parity-bench.json`  
- `vscode-code-search-accuracy.json`  
- Unit: `go test ./internal/codecrawl/`
