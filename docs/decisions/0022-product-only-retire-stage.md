# 0022 — Product-only spine; retire Stage as a product path

## Status

Accepted (2026-07-26).

## Context

ADR 0020/0021 made Go `hosted.Client` the company-doc product runtime but kept
Stage 03–04 (`localauthority`, `codeindex`, `query`, gateway QueryService) as an
intentional dual spine. That dual blocked a single product control plane for
continual ingestion and async gardener.

## Decision

1. **Product is the only brand and the Stage superset.** Company-doc residual,
   codecrawl, exact codeindex, continual, and async gardener live on
   `product-brain`. Peer-auth authority RPCs (Git ingest, vault conversation,
   QueryService, factory/meeting/multimodal/connector/tracer) live under **`product-brain authority`** (`services/gateway/authorityprocess`)
   — not a second product binary.
2. **Stage libraries are product substrate** under `services/` (codeindex,
   query, conversation, ingestion, localauthority, gateway *api). A frozen
   snapshot sits in `archive/2026-07-stage-retirement/` for archaeology.
3. **Async gardener is product-default on local_fs:** durable `gardener.db`;
   authority publish can enqueue to the same queue via
   `OUROBOROS_BRAIN_GARDENER_DB`. `OUROBOROS_BRAIN_ENRICH=sync` for CI.
4. **Continual ingestion is product-owned** for docs (`watch --docs`); Git
   reconcile remains on the authority process; code-watch for working trees.
5. **Benchmarks / full-500 promotion stay held** until explicitly authorized.

## Consequences

- Supersedes dual-spine marketing in ADR 0021 §5.
- Stage closed stack **snapshotted** at `archive/2026-07-stage-retirement/`
  (source archaeology; no Bazel packages). Live libraries restored as product
  substrate; sole binary is `product-brain` (authority via
  `product-brain authority`). See `ARCHITECTURE.md` and
  `docs/specs/product/STAGE-VS-PRODUCT.md`.
- Product owns async gardener + continual ingestion.
- Stage-exit Bazel suites under `tools/stage-*` are retired markers.
- Honest gaps (ACL, vault, Git-object authority, Connect TUI) remain Stage
  archive capabilities — not silent product claims.
