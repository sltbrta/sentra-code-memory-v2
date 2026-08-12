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
`code_ingest_paths`, `code_exact` / `code_defs` / `code_refs` / `code_imports`,
`code_read`, `code_watch`, plus `memory_ask` (company residual) and
`catalog` / `ping`.

Issue #47 adds the bounded local typed operators:

- `memory_put`, `memory_search`, `memory_list`, `memory_promote` — typed
  agent-memory admit/recall/lifecycle over `internal/memory` tiers
  (principal-gated, stm/mtm/ltm). Not the company-doc residual lane.
- `session_continuation` — a single safe composite over `internal/sessionlog`
  that folds the repo-local event stream into a budgeted continuation packet.
  Accepts an optional RFC3339 `now` for deterministic replays. Not the full
  SCM session product.
- `savings_summary` — reads the local `internal/savings` token-savings ledger.

Issue #47 also catalogues deferred/non-goal verbs (`lifecycle_install`,
`session_product`, `code_dense_rerank`, `hosted_tenancy`, `query_advanced`).
They have no handler: calling one returns a structured disclosure
(`ok:false`, `error_code:"deferred"`, `deferred:true`, `decision`, `reason`,
`doc`) instead of an opaque unknown-verb error, and MCP does not expose them
as callable tools.

### code_read (bounded source-region read)

Reads a workspace-relative path with path-traversal, absolute-path, and
symlink-escape rejection. `start_line` defaults to 1; `max_lines` defaults to
200 and is capped at 1,000. Returns `path`, `content`, `start_line`,
`end_line`, and `truncated` (true when the source extends beyond the window).
The response is capped at 1 MiB and individual lines at 1 MiB; an empty window
uses `end_line=start_line-1`. This adapter is intended for trusted local
callers; the local HTTP/MCP surfaces do not provide authentication.

### code_imports (exact import lane)

Equivalent to `code_exact kind=import`. Searches the exact code index for
import declarations matching the query. Returns the same `result` /
`duration_ms` / `search_backend` shape as `code_exact`.

### code_watch (bounded freshness adapter)

Runs the codecrawl `WatchPoll` or `WatchFS` adapter for a finite number of
cycles (default `max_cycles=1`; the JSONL verb does not hang). Collects
refresh events and errors into `events`. Context cancellation is respected.
Each request has a 30-second default wall bound (configurable with `timeout_ms`,
capped at 24 hours) and at most 1,024 returned events.
The CLI streaming watcher (`sentra-code-memory watch`) is unchanged.

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
Broader deferred/non-goal disclosure —
[DEFERRED-AND-NON-GOALS.md](../../../../docs/roadmap/DEFERRED-AND-NON-GOALS.md).
Per-surface parity decision —
[0025-memory-session-lifecycle-parity.md](../../../../docs/decisions/0025-memory-session-lifecycle-parity.md).

## Contracts

`contract.go` holds the canonical typed request/response/error contracts
(`ContractID = sentra-scm.codeserve/v1`), stable error codes, and
`CatalogMetadata()` verb specs (surface, status, fields, aliases). The
`catalog` verb returns the legacy `verbs` list; typed `specs` are opt-in
via `detail: true` to keep the discovery response lean.
`DecodeResponse` binds any
wire response to its typed form; `contract_test.go` proves the typed
contracts match live handler behavior against `testdata/scmfixture`.

## Tests

`go test ./services/brain/internal/codeserve/`
