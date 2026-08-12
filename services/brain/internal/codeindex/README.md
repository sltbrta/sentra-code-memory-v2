# codeindex (stage authority — not the product code CLI)

**Freeze for product features (ADR 0021).** This package backs Stage 03–04
`localauthority` ingestion + ACL-first `query` hydrate.

**Product code search** lives in `internal/codecrawl` and
`product-brain code-*` / `productsearch` profile `code`.

Do not grow product multi-crawler or SCM-parity verbs here; deepen `codecrawl`
instead. Shared extract helpers may be factored only when both layers need them.

## SCIP ingestion boundary (issue #44)

The `DecodeSCIP` and `IngestSCIP` helpers expose a narrow, dependency-free
SCIP decoder that produces typed edges consumed by `codecrawl.IngestSCIP`.
The decoder accepts the canonical `{occurrences:[...]}` and the
`{documents:[...]}` wrapper shape; per-occurrence `range` and
`symbolRoles` integers are mapped to a deterministic `(kind, confidence)`
pair via `roleClassifier`. Unsupported roles degrade to a low-confidence
reference so the ingest never fails on producer variation. The boundary
is intentionally separate from the projection machinery so it can be
extended with new SCIP roles without touching the lexical lane.
