<!-- markdownlint-disable -->


> **Historical snapshot** — see [REMAINING-GAPS.md](../roadmap/REMAINING-GAPS.md) / [memory README](../../services/brain/internal/memory/README.md) for current truth (2026-07-27 residual cortex).

# Deferred SCM + memory E2E close

**Date:** 2026-07-25  
**Status:** shipped on `feat/deferred-scm-memory-e2e` → main

## Deferred SCM (closed)

| Item | Status |
| --- | --- |
| productsearch gob | shipped (#192) |
| code-find-route | shipped (#192) |
| Warm gob v3 | shipped (#192) |
| **fsnotify watch** | **shipped** `WatchFS` + CLI `--fsnotify` |
| **defs/refs CLI** | **shipped** `code-defs` / `code-refs` |
| **multi-verb serve** | **shipped** `product-brain serve` JSON-lines |
| **SCM bake-off harness** | **shipped** `tools/build-spine/scm_codecrawl_bakeoff.py` + receipt |
| Multi-lang symbol heuristics | improved (export class/interface, async def, etc.) |
| Full LSP impact | still heuristic_plus (honest) |
| SCM binary head-to-head | harness ready; receipt `scm_available=false` until `SCM_CLI` set |

## Memory E2E (closed)

| Item | Status |
| --- | --- |
| create → burst → enrich → ask | `TestMemoryLifecycleE2E` |
| Continual delta | same test |
| Session multi-turn ask | `--session` + AnswerOpts |
| LongMem product path | `memory_facts` → OpenMemory + enrich + Ask |
| LongMem mini adapter | passes `memory_facts` to product-brain-eval |
| Gardener on product ingest | already EnrichAfterIngest (#192) |

## Verified smoke

```text
memory_facts: "User lives in Kyoto." → answer cites mem-0, search_mode product_brain_go_memory
```

## Next (SOTA claim only if requested)

Full-500 official judge promotion remains fail-closed until explicitly run.
