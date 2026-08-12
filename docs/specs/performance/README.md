# Performance delivery contract

Status: **[planned] — not shipped.** The values in this contract are targets,
not achieved product claims.

## Required targets

The machine-readable target set (`performance-targets.yaml`, planned and
not checked into this extraction) is: cold
exact/lexical ≤60s; syntax/structure ≤5m; semantic/graph ≤20m; 100-file fast
delta ≤10s; syntax delta ≤60s; p95 exact ≤250ms, dense ≤400ms, hybrid ≤750ms;
first evidence ≤1s; first grounded token ≤3s; and 1,000 concurrent mixed users
while ingestion and gardener work continue.

Measure acquisition, manifest acceptance, processing, and publication as
separate stages. L0–L5 receipts report quality, latency, tokens, cost,
queue/service/retry, resource, and failure information. Target misses block
canonical promotion unless a scoped authorized waiver contains remediation; a
SOTA rank never substitutes for these floors.

## Operating boundary

Local, Modal, and VPS may execute bounded work but never become an untracked
canonical authority. Query capacity stays isolated from ingestion, benchmark,
and gardener work. Every executor run has a TTL and cleanup receipt. Stage 00
starts no heavy run; Stage 10 (planned; not in this extraction) verifies the
company-scale target matrix.

## Acceptance

`just modal-smoke` is the stable bounded execution entry point. Stage 01 proves
only a pure local/Modal logical-receipt parity target. Each later performance or
benchmark stage must self-register its representative shard, pin its manifest,
retain quality/latency/cost/cleanup evidence, and obtain independent review
before making a performance claim.
