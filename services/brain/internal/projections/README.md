# projections

Rebuildable SQLite/store adapters for ontology and dense sidecars, plus the
offline projection propagation SLO contract. `slo_receipt.go` defines
source-specific receipts and deterministic drills for lexical, dense, graph,
claims, cache, and answer surfaces. The query engine requires a receipt
admitter by default and performs a post-admission canonical/fresh-time recheck
at the final emit checkpoint. There is no live organization-brain receipt
source, probe, or receipt datastore; the retired local gateway uses an explicit
legacy opt-out and makes no receipt-enforcement claim.
Derived projections may be absent without changing authority. Opt-in via env
where configured.

`SQLDenseStore` persists model-pinned vectors and provides only a bounded exact
fallback (512 vectors). The residual `dense=sqlite` serving route is the
scope/model/dimension-pinned pure-Go ANN in `internal/dense`; an absent ANN is
diagnosed explicitly and never turns into an unbounded SQLite scan.
