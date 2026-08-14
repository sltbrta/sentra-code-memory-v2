# Deferred and non-goals

This is the canonical disclosure for capabilities the standalone
`sentra-code-memory` product deliberately does not ship. It exists so READMEs
and package docs link to one honest source instead of broken placeholders, and
so the codeserve deferred-verb disclosures have a target.

The product is a **local-first, standalone code-memory and code-intelligence
binary** for coding agents (ADR 0022/0023). Everything below is either deferred
(may land later, not now) or a non-goal (explicitly out of scope).

## Non-goals

These are not defects; they are out of the product's charter.

- **Hosted multi-tenancy and cloud sync.** No managed tenancy, remote overlays,
  canonical base snapshots, tenant-scoped source bundles, or cloud deployment.
  The local HTTP/MCP adapters are loopback, unauthenticated, single-user.
- **Billing and distribution.** No metering, entitlements, or public packaging.
- **Full SCM session product.** Agent continuation packets as a product class,
  and latent development-state memory, belong to the SCM session product, not
  this binary. See `docs/specs/product/SCM-SESSION-PRODUCT.md`. The standalone
  surface is the bounded `session_continuation` composite only.

## Deferred

These are intentionally not implemented in the current surface; invoking the
matching codeserve verb returns a structured `deferred` disclosure.

- **Lifecycle/install/server management** (`lifecycle_install`). Managed server
  start/status, hooks install/uninstall, login/whoami, and uninstall are not
  owned by the standalone CLI.
- **Dense/reranked retrieval** (`code_dense_rerank`). The code lane is local
  heuristic only; dense code vectors and cross-encoder rerank are deferred.
  Local/hosted model clients exist for the memory path but are not wired into
  code retrieval.
- **Prior query modes** (`query_advanced`). Patch plans, test hints, related
  graph/source context, and greenfield/native-first context are not part of the
  standalone operator surface.
- **Compiler/SCIP/LSP authority.** Exact projections are deterministic Go
  lexical/AST; Tree-sitter/SCIP/LSP lanes are deferred (see the code-intelligence
  spec). SCIP ingestion exists as a tested package seam
  (`codecrawl.Index.IngestSCIP` / `codeindex.IngestSCIP`) but is **not wired to
  a codeserve verb**; the default graph stays honest about heuristic authority
  outside Go. See the local-first deferred audit for the planned
  `code_ingest_scip` boundary.

## Local-first deferred capabilities (issue #55)

A cited capability matrix separating local-only, local-plus-optional-LLM, and
inherently-hosted/non-goal features lives in
`docs/research/2026-08-13-local-first-deferred-capability-audit.md`. The
boundaries in force today:

- **Deterministic/local is the default.** No LLM configuration is required for
  any shipped surface; a missing key or transport/parse/policy failure always
  returns the deterministic fallback.
- **Optional Gemini adapter is cloud and memory-lane only.** An opt-in Gemini
  `gemini-3.6-flash` adapter (bounded requests, structured outputs, redaction,
  timeouts, deterministic fallback) may augment query expansion/semantic
  scoring off the critical path. It is planned against the official Google
  Gemini SDK, not raw HTTP or the OpenAI-compatible endpoint (see #60). It is
  **not** on the code index/read/ChangeSet path.
- **SCIP ingestion, session recall, and lifecycle hooks remain unwired.** The
  package seams exist (`IngestSCIP`, `sessionlog.Recall`) but have no codeserve
  verb; they are deferred until measured behind deterministic tests.
- **Dense code retrieval stays deferred.** Pure-Go dense/HNSW exists for the
  memory path only; the code lane is lexical/heuristic.

## Retrieval benchmark lanes

Benchmark claims must state which lane produced them. The standalone product
measures exactly one lane and declares the other two as deferred, never
substituting one for another:

- `local_heuristic` — the lexical/heuristic codecrawl lane. **Measured** by
  `just bench-code` against the checked-in `qafixture` corpus (24 probes).
- `dense_reranked` — dense vectors plus cross-encoder rerank. **Deferred.**
- `compiler_authority` — Tree-sitter/SCIP/LSP authority. **Deferred.**

## What ships instead

The reachable, tested surface is documented in
`services/brain/internal/codeserve/README.md` and the root `README.md`:
index/search/read/exact operators, bounded context packing, typed local
agent-memory operators, the session continuation composite, and the savings
summary — all over CLI/JSONL/HTTP/MCP with conformance tests.

See `docs/research/2026-08-12-parity-audit-and-remaining-work.md` for the full
evidence matrix and `docs/decisions/0025-memory-session-lifecycle-parity.md`
for the per-surface decision.
