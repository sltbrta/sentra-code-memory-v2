# codeserve

**Phase 1** multi-verb JSON protocol for product **code operator** tools
(SCM CLI parity, not session memory).

## Role

- stdin/stdout JSON lines: `{"verb":"code_search","root":"…","q":"…"}`
- Wired by `product-brain serve`
- Warm path: `"no_refresh": true` uses existing gob without full crawl

## Verbs

See `Catalog()` — `code_index`, `code_search`, `code_find_relevant`,
`code_expand`, `code_impact`, `code_find_route`, `code_freshness`,
`code_ingest_paths`, `code_exact` / `code_defs` / `code_refs`, plus
`memory_ask` (company residual) and `catalog` / `ping`.

## Non-goals

SCM session continuation packets —
[SCM-SESSION-PRODUCT.md](../../../../docs/specs/product/SCM-SESSION-PRODUCT.md).

## Tests

`go test ./brain/internal/codeserve/`
