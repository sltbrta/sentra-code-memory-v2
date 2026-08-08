# codeindex (stage authority — not the product code CLI)

**Freeze for product features (ADR 0021).** This package backs Stage 03–04
`localauthority` ingestion + ACL-first `query` hydrate.

**Product code search** lives in `internal/codecrawl` and
`product-brain code-*` / `productsearch` profile `code`.

Do not grow product multi-crawler or SCM-parity verbs here; deepen `codecrawl`
instead. Shared extract helpers may be factored only when both layers need them.
