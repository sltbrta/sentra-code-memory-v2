<!-- markdownlint-disable MD013 -->

# 0023 — Unified product durability and program ladder

## Status

Accepted (2026-07-26).

## Context

ADR 0022 made `product-brain` the sole product binary and Stage libraries
substrate, but left two durability stories (plain residual `local_fs` vs
encrypted authority vault/ACL) and large donor gaps (SCM operator surface,
Lattice lifecycle gardener, multi-tenant, federation). A multi-phase program
must not re-fork the product or regress latency/polish of the residual path.

## Decision

### D1 — Durable product brain model

**Unify (b):** residual IR is a **profiled access path** over vault/keyring/
localstate-capable substrate. Offline single-user is one principal + local
keys on the **same** code paths, not a second engine.

### D2 — Gardener host

**In-process (b):** lifecycle lives in Go `services/brain/internal/gardener`
and `product-brain gardener`. No Lattice Python product daemon.

### D3 — Tenant model order

**Local multi-principal first (b),** then hosted multi-tenant. Cloud tenancy
does not precede ACL/vault unification.

### D4 — Federation unit

**Brain + attenuated capability (c).** Remote brains reauthorize locally;
merge evidence references centrally.

### D5 — SCM non-goals

**Session latent memory and full SCM agent-runtime product scope are out** of
this program. Code operator parity (CLI/MCP/rank/watch) is in.

### Program order

```text
0 spec-lock → 1 SCM code → 2 security unify → 3 gardener lifecycle
  → 4 multi-tenant → 5 federation
```

### Polish and latency (program-wide)

All phases must integrate into the single product brain with production polish
and latency hardening (POL-001–007 in
`docs/specs/product/program/README.md`). Defaults stay lean; costly arms are
budgeted. Performance targets:
`docs/reference/performance-targets.yaml`.

## Consequences

- JSONL session store ceases to be authority of record after Phase 2 cutover
  (export/debug only).
- `path2` remains an ERB/bench adapter, not the multi-tenant product model.
- Specs: `docs/specs/product/program/phase-0*.md` … `phase-5*.md`.
- Plan: `docs/plans/2026-07-26-product-unification-program.md`.
- Supersedes dual-durability marketing; extends ADR 0022 compositionally.

## Rejected alternatives

- (D1a) Keep residual permanently plaintext-only — blocks honest multi-tenant.
- (D1c) Residual read-only projection with writes only via authority process —
  acceptable later optimization; not required for Phase 2 exit if unified open
  path exists.
- (D2c) Lattice sidecar — reintroduces dual runtime.
- Federation or multi-tenant before security unify — security theater.
