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
`code_read`, `code_watch`, `code_repo_map`, `code_structural_search`,
`code_diagnostics`, `code_apply_changeset`, plus `memory_ask` (company
residual) and `catalog` / `ping`.

### code_read (bounded source-region read)

Reads a workspace-relative path with path-traversal, absolute-path, and
symlink-escape rejection. `start_line` defaults to 1; `max_lines` defaults to
200 and is capped at 1,000. Returns `path`, `content`, `start_line`,
`end_line`, and `truncated` (true when the source extends beyond the window).
The response is capped at 1 MiB and individual lines at 1 MiB; an empty window
uses `end_line=start_line-1`.

Path policy (issue #41): after the non-bypassable safety rejections above, a
read must also survive the repository ignore policy (`internal/repoignore`)
and — when a durable index exists at `index_cache` or the default
`.sentra/code-index.gob` — index membership. Violations fail with
`error_code: "path_denied"`; a corrupt index fails closed as
`index_unavailable`. The typed opt-in fields `allow_ignored` and
`allow_unindexed` (reachable on CLI/JSONL/HTTP/MCP) restore the legacy
behavior explicitly. Without a durable index the ignore gate is the only
membership policy (compatibility fallback). The local HTTP trust policy is
specified in `internal/adapters` (loopback default, optional bearer token).

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

## Agent code-intelligence and mutation surfaces (#45–#46)

- `code_repo_map` returns task-personalized file and symbol PageRank plus an
  Aider-style map constrained by `max_bytes` / estimated `max_tokens`. Direct
  query hits retain a 40% context floor. `mode=fast|quality|deep` controls
  bounded candidate and iteration defaults.
- `code_structural_search` is a deterministic text-pattern rule lane. `$NAME`
  metavariables match identifiers. Results are file/match/byte bounded and
  explicitly report `authority=heuristic`; they are not AST or compiler truth.
- `code_diagnostics` reports real index graph/symbol counts and detected build
  manifests/commands without claiming that a compiler or build ran.
- `code_apply_changeset` decodes the typed workflow ChangeSet, stages and
  verifies it through `workflow.ApplyChangeSet`, promotes only a complete
  candidate, refreshes the code index when possible, and returns a content-safe
  receipt. Direct CLI usage reads the ChangeSet from `--changeset FILE`.

All four verbs are in `CatalogMetadata()`, JSONL, direct CLI, HTTP dispatch, and
MCP tools/list/tools/call. The local mutation surface assumes a trusted caller;
HTTP remains unauthenticated and should remain loopback-only.

## Non-goals

SCM session continuation packets —
[SCM-SESSION-PRODUCT.md](../../../../docs/specs/product/SCM-SESSION-PRODUCT.md).

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
