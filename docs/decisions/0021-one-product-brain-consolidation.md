# 0021 — One product brain; port then archive duals; benches only via product

## Status

Accepted (2026-07-25).

## Context

ADR 0019 (ERB scores product) and ADR 0020 (product path is Go) already
fixed ownership, but dual answer engines remained in-tree: Python
`product_brain` / `live` retrieval stacks, optional pack hop-0, and soft
opt-in for LongMem/Terminal. Local and hosted looked like two products
rather than store adapters of one `hosted.Client`.

Port-gap analysis (2026-07-25) found residual hybrid retrieve/answer
largely already in Go; remaining must-port items were authority/recency,
best_last packing, prompt modes/fact slots, product chat (sessions +
turn_grep), multi-provider synth fallback, and quality-only query expand.

## Decision

1. **Single product brain** = Go `services/brain` surfaces:
   - company-doc: `hosted.Client` with store adapters only
     (`local_fs` | `memory` | `product_neon` | `path2`)
   - product code: `codecrawl` via `product-brain code-*` / `productsearch` profile `code`
   - facade: `productsearch` + `product-brain` CLI + `product-brain-eval`
2. **Local and hosted are interchangeable backends**, not separate products.
3. **Port before archive.** Dual Python engines move to
   `archive/2026-07-product-brain-consolidation/` only after must-port
   algorithms land in Go (or are explicitly accepted as quality-only /
   harness-only in the ported-algorithms note).
4. **Every official benchmark** answers only through `product_adapter` →
   `product-brain-eval` / product HTTP. Pack hop-0 is diagnostic-only,
   never promotion authority.
5. **`codeindex` + Stage 04 `query`** remain stage authority; product code
   features deepen `codecrawl`, not a second product code brain.
6. **Python** may keep judges, scorers, export, Modal orchestration — never
   a parallel product retrieval/synthesis runtime.

## Consequences

- Plan: `docs/plans/2026-07-25-one-product-brain-consolidation.md`
- Research: `docs/research/2026-07-25-product-brain-port-gap.md`
- Env: official paths do not depend on `OUROBOROS_ERB_PRODUCT_GO` opt-in;
  legacy SQLite dual-key escape is removed after archive.
- SOTA / leaderboard claims remain fail-closed until official judges on
  this product path pass the promotion gate.
