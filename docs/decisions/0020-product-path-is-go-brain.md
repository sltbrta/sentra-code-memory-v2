# 0020 — Product path is the Go brain; ERB Python is harness only

## Status

Accepted (2026-07-24).

## Context

Stage 09 EnterpriseRAG work built a capable Python path under
`workers/eval-media/.../enterprise_rag_bench` (and a `product_brain` package)
with hybrid retrieval, sessions, multi-hop, ANN experiments, and live LLM
answering. In parallel, the real product spine is `services/brain` (Go):
event/evidence authority, code ingestion, Stage 04 grounded query, OpenFGA-
compatible authorization, conversation vault — without company-doc ontology,
gardener, or hybrid dense product retrieval.

Treating the Python ERB stack as “the product brain” forked the architecture:
benchmarks did not measure the product; ontology and gardener stayed deferred.

## Decision

1. **The product brain is exclusively `services/brain` (Go)** plus gateway/TUI
   surfaces. All SOTA retrieval, ontology, gardener, and company-doc work
   lands there.
2. **Python EnterpriseRAG code is an evaluation harness** (adapters, judges,
   Modal runners, evidence). It may call the product brain; it must not grow
   a parallel retrieval/synthesis product.
3. **Ingest-time LLM enrichment is allowed** under performance-targets.yaml
   budgets, as async gardener work after `retrieval_ready`.
4. **SOTA claims** require official/adapter scores on the **product** path
   only; promotion remains fail-closed.

## Consequences

- Plan: `docs/plans/2026-07-24-product-brain-sota-path.md`.
- `docs/specs/brain/ENTERPRISE-PROFILE.md` describes product profiles on the
  Go path; Python package is labeled eval-only.
- New features in `eval-media/.../live` and `product_brain` are harness-only
  unless they are thin adapters to Go.
- Full-500 / LongMem / Terminal-Bench re-runs wait until product adapters exist.
