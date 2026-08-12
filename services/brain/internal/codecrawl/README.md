# codecrawl

Working-tree **code operator** index (SCM repo tools, not session memory).

## Capabilities

- Multi-worker crawl → durable `code-index.gob`
- Stamp+hash warm refresh (zero body read when stamps match)
- Search, find-relevant, expand, impact, find-route (heuristic authority)
- Search ranking uses rare-term weighting and query-term coverage so repeated
  generic tokens do not drown out files matching the complete request.
- Watch: fsnotify or poll
- One shared ignore policy reads root `.gitignore`, `.dockerignore`, and
  `.git/info/exclude`, plus conservative generated/secret defaults. Useful
  configuration such as `.github` remains searchable unless explicitly ignored.

## Phase 2 typed graph (issues #13, #17)

The Phase 2 vertical slice adds a deterministic, bounded typed-edge
projection over each indexed file:

- `Edge` records carry `Kind` (`call`, `reference`, `import`,
  `implementation`, `inheritance`, `lexical`), `Authority`
  (`ast`, `heuristic`, `lexical`), `Confidence`, and `Provenance` with
  file/line/column/parser/language/snippet. The `Target` field is reserved
  for unresolved import paths and is omitted when the edge resolves
  locally.
- Go files use `go/parser`; non-Go files fall back to a lexical identifier
  scan (def/ref pairs) so the projection is never silently empty. The
  fallback tags every edge `Authority: lexical` so callers can branch on
  authority without parsing the parser label.
- Edges are capped per file (default `MaxEdgesPerFile = 512`); the cap is
  exposed via `SetEdgeCap` for tests that need deterministic truncation.
  BFS helpers (`callersFor`, `calleesFor`, `fileEdgesByImportStem`) iterate
  sorted file keys so impact closures produce the same answer on every
  invocation, regardless of Go map iteration order.
- The projection is persisted in the durable gob (schema v4) and rebuilds
  lazily from `fileEdges` on first `Graph()` access. Older snapshots
  decode cleanly: the missing `FileEdges` field becomes an empty map, and
  `Index.HasGraph()` returns false so callers degrade to the Phase 1
  name-graph heuristic.

## Phase 2 ImpactReceipt (issue #17)

`ImpactReceipt` keeps every Phase 1 JSON field byte-compatible and adds:

- `truncated`: explicit cap-hit signal (call-aware selection hit the
  per-symbol cap, or the closure BFS hit `maxFiles`).
- `severity`: deterministic `low` (≤4), `medium` (≤16), or `high`
  (>16), bucketed against the larger of `Direct` and `Closure`.
- `affected_tests`: the sorted, deterministic test-file subset of
  `Closure` (`_test.go`, `.test.`, `.spec.`, `/tests/`, `/__tests__/`).
- `changed_symbols`: bounded list of `defs` from a file seed so callers
  can pivot to a more specific seed.
- `schema: "v2"` and a fail-closed `graph_unavailable` note when the
  typed-edge projection is absent. Empty seeds emit `unknown_seed`.

`codeserve.ImpactResponse` keeps wrapping the same `codecrawl.ImpactReceipt`
struct, so the additive fields surface on the wire without breaking
existing JSON consumers.

## CLI

`product-brain code-index|code-search|code-watch|…`

Exact P5 defs/refs live in sibling **codeindex** (`code-exact`).

## Non-goals

Session latent memory —
[SCM-SESSION-PRODUCT.md](../../../../docs/specs/product/SCM-SESSION-PRODUCT.md).
Git-object authority for company ACL code is **ingestion** + authority process.
