# Brain Enterprise Profile (product Go path)

Status: **[partial — product company-doc path is Go `hosted.Client`; harness is
eval-only]** (ADR 0020 / 0021).

Authority: [ADR 0020](../../decisions/0020-product-path-is-go-brain.md),
[ADR 0021](../../decisions/0021-one-product-brain-consolidation.md),
plans [SOTA](../../plans/2026-07-24-product-brain-sota-path.md) +
[consolidation](../../plans/2026-07-25-one-product-brain-consolidation.md).

## Product vs harness

| Path | Role |
| --- | --- |
| `services/brain` (Go) `hosted` / `productsearch` / CLI | **Product** company-doc + code (`codecrawl`) |
| Gateway + TUI | User-reachable Ask/Sources/History/Status |
| `workers/eval-media/.../product_adapter` + judges | **Eval harness only** |
| `archive/2026-07-product-brain-consolidation/` | Retired Python dual engines |

## What is true now

- Stage 04 grounded query over **committed-Git code** corpus (authority).
- **Company-doc product path:** `hosted.Client` store adapters
  (`local_fs` \| `memory` \| `product_neon` \| `path2`), residual_parity stack,
  HotLex interactive, ERB via `product-brain-eval`.
- **Memory cortex:** light ingest seed; async `RunCortexMaintenance`; ask rank
  arms (utility, PPR, PageIndex, RAPTOR, agent tiers) under `internal/memory`.
- Product code path: `codecrawl` + `code-exact` (P5 `codeindex`).
- Python dual `product_brain` package **archived** after port gate.

## Residual path (shipped, lean)

```text
ingest → retrieval_ready (lexical)
  → light memory seed (utility + texts + episode)
  → async gardener enrich (doc2query / edges / context / dense)
  → post-wave RunCortexMaintenance
       (extract · edges · PageIndex TOC · global PR prior · community)
  → ask: multi-arm IR + claim/utility/global_pr/PPR/pageindex/RAPTOR
       + optional agent STM/MTM/LTM → ground → dual-cite
  → lifecycle optional: C1 · NREM · det. REM · RAPTOR · reseg
```

Authority ACL Ask remains `product-brain authority` (dual plane by design).
LLM REM / LLM PageIndex walk / SCM session are non-goal or deferred.

## Target product pipeline (SOTA-shaped ambition)

```text
ingest → retrieval_ready (deterministic lexical)
  → gardener (concurrent LLM: doc2query, entities, edges, dense, summaries)
  → residual cortex + optional denser OpenIE / LLM walk arms
  → query: auth → hybrid BM25+dense RRF → CE → graph hop/PPR
  → corrective if weak → hydrate → synthesize claims → ground → history
```

Enrichment LLM is **async after retrieval_ready**, concurrent, budgeted to
`docs/reference/performance-targets.yaml` (semantic/graph cold ≤ 20 min).

## Profiles (product)

| Profile | Session history | Graph hop | Dense hybrid | Pack hop-0 |
| --- | --- | --- | --- | --- |
| `product` | on | on | on | **off** |
| `bench` | off (stateless) | on | on | **off** |

## Non-claims

No SOTA/leaderboard until product-path official/adapters pass promotion gate.
