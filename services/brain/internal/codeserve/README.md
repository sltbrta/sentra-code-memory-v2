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

## Contracts

`contract.go` holds the canonical typed request/response/error contracts
(`ContractID = sentra-scm.codeserve/v1`), stable error codes, and
`CatalogMetadata()` verb specs (surface, status, fields, aliases). The
`catalog` verb returns the legacy `verbs` list; typed `specs` are opt-in
via `detail: true` to keep the discovery response lean.
Planned verbs (`code_read`, `code_imports`, `code_watch` over JSONL) are
typed now and gain handlers in later phases. `DecodeResponse` binds any
wire response to its typed form; `contract_test.go` proves the typed
contracts match live handler behavior against `testdata/scmfixture`.

## Tests

`go test ./services/brain/internal/codeserve/`
