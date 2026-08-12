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

## Durable persistence (issue #42)

`Index.Save` / `Load` guarantee old-or-complete-new state, never partial:

- **Coordination:** a per-path in-process mutex plus an advisory flock on
  `<gob>.lock` (darwin/linux; tmp+rename alone elsewhere). Save fails closed
  when the lock cannot be taken; Load locks best-effort so a read-only
  directory cannot make a valid gob unreadable.
- **Durability chain:** the temp file is fsynced before the atomic rename and
  the parent directory is fsynced after it (directory-sync EINVAL/ENOTSUP
  tolerated, everything else fails).
- **Crash recovery:** a stale `.tmp` from an interrupted writer is discarded
  before each Save; a corrupt gob fails `Load` and `OpenOrRefresh` recovers
  by reindexing.
- **Root binding:** `DurableMeta.Root` is validated symlink-aware
  (`ValidateRoot`, `ErrRootMismatch`). `OpenOrRefresh` reindexes and rebinds
  on mismatch; read paths (codeserve `no_refresh`, `code_freshness`,
  `code_ingest_paths`) fail clearly instead of serving another workspace's
  index.

## Phase 2 typed graph (issues #13, #17)

The Phase 2 vertical slice adds a deterministic, bounded typed-edge
projection over each indexed file:

- `Edge` records carry `Kind` (`call`, `reference`, `import`,
  `implementation`, `inheritance`, `lexical`, `definition`),
  `Authority` (`ast`, `heuristic`, `lexical`, `scip`), `Confidence`,
  and `Provenance` with file/line/column/parser/language/snippet. The
  `Target` field is reserved for unresolved import paths and is omitted
  when the edge resolves locally.
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

## SCIP ingestion (issue #44)

`Index.IngestSCIP` accepts a parsed `codeindex.SCIPDocument` (see the
boundary in `codeindex/scip.go`) and merges the typed edges into the
per-file map with `Authority: scip`. The pipeline is:

- A codeindex SCIP boundary parses canonical SCP JSON or the
  `{documents:[...]}` wrapper shape; the per-occurrence `range` and
  `symbolRoles` integers are mapped to a deterministic `(kind,
  confidence)` pair via `roleClassifier`.
- The codecrawl IngestSCIP replaces existing SCIP edges for the same
  path (idempotent re-ingest) and demotes the AST/heuristic edges that
  conflict with the same target. Lexical edges are preserved as the
  bounded fallback.
- `AuthorityRank` orders authorities (SCIP > AST > heuristic > lexical)
  so the rank fusion can resolve conflicts deterministically.
- Unsupported SCIP roles degrade to a low-confidence reference rather
  than failing the ingest. The conversion keeps the
  `Provenance.Parser = "scip"` label so callers can introspect the
  lane without inferring it from the authority enum.

## Hybrid retrieval and ranking (issue #43)

`Index.FindRelevantRanked` runs the code-aware hybrid pipeline:

- Lexical baseline: the existing `SearchOpts` ranks the candidate pool
  with TF-IDF, path penalties (`pathRankMultiplier`), and the interface
  query boost.
- Identifier floor: defining files cannot be removed by MMR or
  thresholding. The floor is bounded by `IdentifierFloorCap` so callers
  cannot accidentally pin the entire result set.
- Graph fusion: when the typed-edge graph is present, the pipeline
  computes a deterministic normalised PageRank (damping 0.85, up to 32
  iterations, L1 convergence at 1e-6) and a per-file degree signal.
  Both are bounded and surfaced on the diagnostic.
- Rerank: the optional `Reranker` interface plugs in a cross-encoder or
  embedding ranker. The fallback is a deterministic MMR pass over the
  fused candidates so the pipeline never requires credentials.
- Diagnostics: the `RankedAgentPayload` envelope carries the candidate
  breadth, the top PageRank/degree pairs, the fusion weights, the
  identifier floor hits, and the rerank strategy. Callers can branch on
  the `graph_unavailable` note when the typed-edge projection is absent.

Headline hit@1/5/10 acceptance tests live in `ranking_hitatk_test.go`
and `ranking_test.go`. The benchmark tests in `ranking_benchmark_test.go`
complement the unit tests with timer-based coverage.

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
