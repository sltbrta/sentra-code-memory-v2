# gardener

Async **enrichment** and **lifecycle consolidation** after `retrieval_ready`.
Package doc: [doc.go](doc.go). Cortex mutations: [memory/](../memory/README.md).

## Modes

| Mode | CLI / hook | What |
| --- | --- | --- |
| Enrich wave | `product-brain gardener --dir … [--once\|loop]` | doc2query / edges / context / dense workers → WarmSidecars |
| Post-wave cortex | product-hosted after `RunGardenerWave` | `memory.RunCortexMaintenance` when Mem attached (not a queue job) |
| Lifecycle | `gardener --dir … --lifecycle [--rem]` | C1 → cortex → utility → NREM → [opt] REM → RAPTOR → hyp/prune → reseg |
| Auto drain | `OUROBOROS_BRAIN_GARDENER_AUTO=1` | Background enrich+cortex after OpenLocal |

## Queue substrates

| Impl | When | Parallelism |
| --- | --- | --- |
| `SQLiteQueue` | Default solo (`Dir/gardener.db`) | Multi-reader WAL; **single writer** |
| `PostgresQueue` | `OUROBOROS_BRAIN_QUEUE=postgres` + DSN | Multi-worker `FOR UPDATE SKIP LOCKED` |
| `MemoryQueue` | Tests / no Dir | Process-local only |

Local residual sizes drain with `LocalWorkerBudget(OUROBOROS_BRAIN_WORKERS|GOMAXPROCS)` —
local workers **in lieu of hosted burst fleet**.

## Invariants

- Never mutates primary evidence (`chunks.jsonl` digest stable — GDN-002)
- Never blocks lexical admit
- Durable queue required for residual/hosted with Dir (memory→sqlite alias)
- Queue workers may emit **receipt stubs**; product-brain CLI / `memory` owns
  real cortex mutations

## Cortex (heavy, off ingest hot path)

Ingest seed stays **light**: utility + doc texts + episode bind.

`RunCortexMaintenance` (post-wave on **async** `RunGardenerWave` and sync enrich):
claim extract → **`SeedRelationsFromClaims`** (TemporalRelations left-shift) →
prose co-occur edges → PageIndex TOC → global PageRank → community/RAPTOR.
Ask then ranks with utility, claim prefer, global_pr, PPR, pageindex, RAPTOR
inject, optional agent memory — lean serve walks precomputed relations only.

## Lifecycle order (CLI `--lifecycle`)

1. C1 predict-calibrate may skip heavy work  
2. `RunCortexMaintenance` if not already warm  
3. Utility half-life decay  
4. NREM low-utility quarantine (no chunk delete)  
5. REM — opt-in **deterministic** re-extract (`--rem` / `OUROBOROS_BRAIN_REM=1`)  
6. RAPTOR refresh (community nodes preserved)  
7. Claim-linked edges + C5-light hypothesize / weak prune  
8. Episode reseg (`LifecycleResegment`)

## Partial / non-goals

- Deterministic REM shipped; **LLM REM** is non-goal (NG-REM-LLM flag scaffold only)
- No Lattice persona / smart-folder product
- No Python SMF gardener process
