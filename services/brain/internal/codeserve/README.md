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

## Bounded context packing (opt-in)

`code_find_relevant` accepts optional `max_bytes`, `max_tokens`, `render`
(`full|signatures|skeleton|compact`), and `session` fields. When any of
`max_bytes` / `max_tokens` / `render` is set, the response adds a `context`
block from `internal/contextpack`: hard byte/token budgets with
relevance-proportional allocation and a direct-source floor, explicit
truncation/omission metadata, stable expansion handles (stale handles fail
clearly), and — when `session` is set — cross-call dedup back-pointers for
unchanged source. A fail-safe governor (candidate + output-byte limits) is
reported in `context.meta.governor`. When the fields are unset the response
is byte-identical to the legacy payload.

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
