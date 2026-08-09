# Production latency stack

> **Historical snapshot** — see
> [REMAINING-GAPS.md](../roadmap/REMAINING-GAPS.md) / [memory
> README](../../services/brain/internal/memory/README.md) for current truth
> (2026-07-27 residual cortex).

**Date:** 2026-07-25  
**Stack stamps:** `residual_parity_v2_prod` (default batch) ·
`residual_parity_v2_prod_interactive` (HotLex serving)

## Why

Residual-v2 full500 OK cells were multi-minute: **Neon FTS multiplies with every
variant**.
Hard-type lean override forced 3× FTS on semantic — primary timeout driver (215
@ 480s).

Prod caps cut fan-out; **interactive G15 still hits Neon GIN physics** (~1.55M
chunks).
Solution is not more FTS variants — it is a **hot serving index +
hydrate-by-id**.

## Production defaults (`OUROBOROS_ERB_PROD=1`)

| Lever | Prod | Quality (`OUROBOROS_ERB_QUALITY=1`) |
| --- | --- | --- |
| Lex variants | up to 2; missing HotLex cap 1 | up to 3 parallel; missing HotLex cap 1 |
| Lex OR terms | 12 | 14 |
| Lex timeout | 2s/query | 2.5s/query |
| Dense queries | up to 2 (parallel with lex) | up to 2 (parallel with lex) |
| Structure FTS rescue | off (pool-only) | pool-only by default |
| Hydrate | 3×2 / multi 5×3 | 4×2 / multi 6×4 |
| Corrective re-retrieve | off by default | off by default |
| Agentic | env, default on | env, default on |
| Synth retries | 1 | 1 |

## Interactive class (G15) — HotLex

When `HotLex` is warm (or `OUROBOROS_ERB_INTERACTIVE=1`) and not QUALITY mode:

1. **Phase A** (≤2–2.5s live shared wall): HotLex BM25 + dense ANN + eligible
   FTS share one caller-derived context and run in parallel → RRF; variant count
   does not multiply the wall
2. **Hot projection gate:** a large usable HotLex projection skips Neon unless
   `OUROBOROS_ERB_FORCE_FTS=1`
3. **Missing projection:** exactly one Neon FTS variant, hard-capped at 3s even
   under official BENCHMAX conflicts
4. **Hydrate-by-id** from Neon/path2 for missing text (source of truth)
5. Structure pool-only + light CE + coverage window

Local/memory: `interactive_local` (HotLex + store lexical fallback +
structure).

Soft-empty stopword queries return empty passages (not Failure).

| Env | Role |
| --- | --- |
| `OUROBOROS_ERB_HOTLEX_PATH` | Load gob projection at `Open` |
| `OUROBOROS_ERB_FORCE_FTS=1` | Force the eligible Neon arm; does not remove its deadline |
| `OUROBOROS_ERB_FORCE_RESIDUAL=1` | Explicit residual multi-arm ablation; diagnosed, never selected silently |
| `OUROBOROS_ERB_QUALITY=1` | Wider budgets within the same interactive product route |

Request cancellation is inherited by dense/FTS fanout, path2 structure arms,
project hop-2, and hydration. Each fanout shares one stage wall. A valid empty
FTS result remains a soft empty; diagnostics distinguish `missing`, `timeout`,
`partial_failure`, `empty`, and `residual_opt_in`. A zero FTS budget is labeled
`caller_deadline_only` and is permitted only for explicit non-official BENCHMAX.

Project tooling:

```text
product-brain project-hotlex --dir <brain> | --jsonl chunks.jsonl --out hotlex.gob
python tools/build-spine/project_hotlex_path2.py   # dumps path2 JSONL for gob
```

Local dir layout: `meta.json`, `chunks.jsonl`, `sidecars.jsonl`,
**`hotlex.gob`**.

## Still open

- Full path2 HotLex projection size/RAM for 1.55M chunks (cap via
  `OUROBOROS_ERB_HOTLEX_MAX_DOCS`).
- G15 p95 ≤750ms **measured** on Modal with hot gob mounted — code path is in;
  re-bench env-gated.
- Residual timeout@900s for last full500 misses + official judge (separate from
  HotLex).
