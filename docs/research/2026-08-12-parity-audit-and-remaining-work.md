<!-- markdownlint-disable MD013 MD060 -->

# Post-P6 parity audit and remaining work

- **Status:** answered with implementation backlog
- **As of:** 2026-08-12, after PR #40 (`92bfb8a`); reconciled 2026-08-14 under issue #54 (see “Reconciliation” below)
- **Informs:** next parity implementation stack and release scope
- **Compared against:** prior local SCM implementations, the extracted Go product path, and relevant code-intelligence/agent-memory projects

## Executive answer

The P6 local operator contract is now shipped: CLI/JSONL/HTTP/MCP share 17
codeserve verbs, including bounded `code_read`, exact `code_imports`, and
bounded `code_watch`. The remaining work is not another small verb gap. It is
mostly **authority, retrieval quality, security, and reachability**:

1. secure the unauthenticated local HTTP/MCP trust boundary and make reads obey
   ignore/index policy;
2. add durable crash/process safety to the working-tree index;
3. improve code retrieval with identifier guarantees, code-aware dense/rerank
   lanes, and a ranked repo map;
4. replace non-Go lexical graph fallback with Tree-sitter/SCIP/LSP authority;
5. expose or explicitly retire the existing session, memory, savings, and
   workflow packages from the canonical agent surface;
6. implement the transactional ChangeSet apply engine; and
7. restore missing release/roadmap docs and add repeatable cross-client and
   retrieval benchmarks.

Hosted cloud tenancy, billing, remote overlays, and enterprise deployment remain
explicitly deferred rather than parity defects.

## Reconciliation (issue #54, 2026-08-14)

The matrix below was written on 2026-08-12, before several follow-up slices
landed. Rows marked **stale** in this note have since shipped; read their
“Status” cells as superseded by the following:

- **Ranked retrieval** (was “Missing or partial”): opt-in `code_find_relevant`
  now returns a deterministic identifier-floor/PageRank/MMR `ranked_payload`
  with `affected_tests` (commit `d331c9b`).
- **Repo map** (was “Missing”): `code_repo_map` ships a task-personalized,
  token-budgeted file/symbol PageRank map (`mode=fast|quality|deep`).
- **Structural search** (was “Missing”): `code_structural_search` ships a
  bounded deterministic heuristic pattern lane with explicit
  `authority=heuristic` (not ast-grep/compiler truth).
- **Diagnostics** (was “Missing”): `code_diagnostics` ships real index
  graph/symbol counts and detected build metadata without claiming a compiler
  ran.
- **Transactional ChangeSet application** (was “Missing”, “spec says
  planned”): `code_apply_changeset` ships staging/verify/promote with
  stale-base and path-escape rejection; the code-intelligence spec status is
  now “partially implemented” (bounded engine shipped, compiler-grade target
  still planned).
- **Session continuation packets** (was “no codeserve verb”):
  `session_continuation` ships a bounded repo-local continuation composite;
  the full SCM session product remains deferred.
- **Typed agent-memory tools** (was “only `memory_ask`”): `memory_put` /
  `memory_search` / `memory_list` / `memory_promote` ship over the canonical
  surface.
- **Savings/benchmark evidence** (was “not a complete quality gate”):
  `savings_summary` ships, and `just bench-code` runs a deterministic 24-probe
  offline retrieval gate with a checked-in baseline.
- **HTTP/MCP auth and filesystem trust** (was “P0 security”): `code_read` is
  now ignore/index-gated with `allow_ignored`/`allow_unindexed` opt-outs, and
  the HTTP adapter supports an optional bearer token; the surface remains
  loopback-only and unauthenticated by default.
- **Docs and contract links** (was “defect”, “`StatusPlanned` is now unused”):
  the broken links and the unused `StatusPlanned` enum/comments were removed.

Rows that remain accurate are left untouched: dense/reranked code retrieval,
Tree-sitter/SCIP/LSP authority, lifecycle/install, index crash/process safety
and hosted tenancy are still deferred or partial as written. Out-of-extraction
references in “Source inventory” are classified below.

## Evidence and parity matrix

| Area | Current evidence | Prior/competitor evidence | Status | Priority |
|---|---|---|---|---|
| Local index, stamp/hash refresh, ignore-aware crawl, durable gob, fsnotify/poll watch | `services/brain/internal/codecrawl/`, `codeserve` tests | Prior SCM `scm-v2/`; current `docs/research/2026-07-25-codecrawl-scm-parity.md` | Shipped local baseline | — |
| CLI/JSONL/HTTP/MCP equivalence | `services/brain/internal/codeserve/`, `services/brain/internal/adapters/`, PR #40 | Previous contract matrix; local MCP/HTTP tests | Shipped, but trust boundary remains | P0 security follow-up |
| Exact defs/refs/imports | `services/brain/internal/codeindex/`, `codeserve` exact lane | Prior SCM P5 projections | Shipped; bounded and honest about authority | — |
| Heuristic search/relevant/expand/impact/route | `services/brain/internal/codecrawl/verbs.go` | SCM parity research and prior route tools | Shipped heuristic; not compiler-grade | P1 |
| Code dense retrieval and cross-encoder rerank | No dense/rerank use in `codecrawl`; `docs/benchmarks/vscode-qa.md` calls out semantic/rerank need | Prior SCM Cohere/ZE code retrieval; Continue reranker pipeline; Copilot semantic indexing | Missing in code lane | P1 |
| Exact identifier boost/representatives, PageRank fusion, file-stem and path-class penalties, focus lane | Prior v1↔v2 audit `.claude/v1-v2-parity-audit.md` gaps 1–7; current codecrawl has only heuristic boosts | Prior SCM rank fusion and Aider PageRank repo map | Missing or partial → Shipped (ranked); see Reconciliation | — |
| Typed graph authority across languages | Go `go/parser`; non-Go lexical fallback; `codecrawl/README.md` | Prior SCM syntax graph; Sourcegraph SCIP; Serena LSP; ast-grep Tree-sitter | Partial | P1 |
| Repo map / task-personalized symbol overview | No `code_repo_map` catalog verb; contextpack is bounded retrieval, not a map | Aider repo map uses Tree-sitter + PageRank | Missing → Shipped; see Reconciliation | — |
| Structural search and rewrite | No ast-grep/semgrep-style verb | ast-grep MCP (`find_code`, `find_code_by_rule`, syntax tree) | Missing → Shipped (heuristic); see Reconciliation | — |
| Diagnostics/compiler/build/dependency lane | No LSP/SCIP/build graph in code operator; spec says these are required lanes | Serena LSP; Sourcegraph precise navigation/SCIP | Missing → Shipped (heuristic); see Reconciliation | — |
| Transactional ChangeSet application | `services/brain/internal/workflow/changeset.go` plus `code_apply_changeset` (stage/verify/promote) | Existing authority factory pipeline; spec transaction flow | Shipped bounded engine; full transaction flow still planned | — |
| Context budgets, render modes, handles, session dedup | `services/brain/internal/contextpack/`; bounded tests | Aider map budgets; Continue retrieval/context pipeline | Shipped as opt-in `code_find_relevant` | — |
| Savings ledger and benchmark evidence | `services/brain/internal/savings/`, `scmbench/` | Prior token-savings program issues #5–#11/#24 | Implemented packages; not a complete quality gate → Shipped (`savings_summary` + bench gate); see Reconciliation | — |
| Session continuation packets and bounded recall | `services/brain/internal/sessionlog/` exists and is tested, but no codeserve verb; docs call session product deferred | Prior SCM memory tools and session APIs | Reachability/doc conflict → Shipped; see Reconciliation | — |
| Typed agent-memory tools | Current canonical surface exposes only `memory_ask`; no add/search entity/fact/preference/trace tools | Prior SCM 23-tool contract and MCP `memory-*` tools | Missing from standalone operator surface → Shipped; see Reconciliation | — |
| Memory/cortex projections | `services/brain/internal/memory/` and product path APIs exist | Prior SCM typed recall, Graphiti temporal graph, Letta memory blocks/archival memory | Core exists; adapter/contract incomplete | P2 |
| Lifecycle/install/server management | No install/service/hook/uninstall lifecycle in standalone CLI | Prior SCM `server`, `index`, `login`, `hook`, `uninstall`, hosted freshness/dirty-sync | Missing; decide whether standalone owns it | P2 |
| Index crash/process safety | `codecrawl/persist.go` atomic publication; no confirmed file lock/fsync/root binding | Product localstate durability patterns; ADR 0023 | Partial | P1 |
| HTTP/MCP auth and filesystem trust | `services/brain/internal/adapters/http.go` and MCP are local adapters; no auth; `code_read` is root-bounded but not ignore/index-gated | Prior authority security contracts and gateway local authority | Security hardening needed; local-only trust must be explicit → Shipped partial (read gating + bearer); see Reconciliation | — |
| Language breadth | Exact lane covers Go/TS/Python/Rust/Java; graph lane degrades lexically outside Go | Prior SCM Tree-sitter/SCIP work; Serena advertises 40+ LSP languages | Partial | P1 |
| Benchmarks and regression gates | `docs/benchmarks/vscode-qa.md`, `scmbench`; no repeatable 50-query code QA gate or full competitor-equivalent task runs | Prior SCM roadmap: Terminal-Bench, DeepSWE/Pier, SWE-Bench gates | Partial/deferred → Partial (24-probe gate); see Reconciliation | P2 |
| Docs and contract links | `services/brain/README.md`, roadmap, product specs, and ops profile paths | Prior roadmap and parity docs | Reconciled under issue #54 (links repaired, `StatusPlanned` removed) | — |
| Hosted multi-tenant, billing, cloud deployment, remote overlays | Explicit ADR 0023/README non-goals | Prior SCM hosted roadmap | Deferred intentionally | P3/deferred |

## Prior implementation parity: what was lost or narrowed

> **Out-of-extraction reference.** The three paths named below are historical
> references to the pre-extraction Ouroboros/Sentra source tree. They are not
> part of this repository and are not resolvable from it; they are cited only
> as provenance for the parity comparison, not as runnable code.

The prior SCM repositories (`/Users/sammy/Developer/Sentra_Research/sentra-code-memory`,
`/Users/sammy/Developer/Sentra/sentra-code-memory`, and
`/Users/sammy/Developer/Sentra/code-memory-tokoyo`) expose a broader product
shell than this standalone Go extraction. The notable differences are:

- lifecycle/operator commands: managed server start/status, local status,
  hooks install/status/uninstall, uninstall, login/whoami, hosted freshness and
  dirty-sync paths;
- query modes beyond the current lexical/exact operator verbs: patch plans,
  repo reads/search, test hints, status, related graph/source context, and
  greenfield/native-first context;
- a 23/24-tool memory-compatible MCP contract with typed recall tools for
  entities, facts, preferences, relationships, messages, and reasoning traces;
- hosted worktree overlays, canonical base snapshots, auth, and tenant-scoped
  source bundles; and
- stronger code ranking and syntax/semantic lanes: dense code vectors,
  Cohere/ZE rerank, exact identifier boosts, personalized PageRank, and
  Tree-sitter/SCIP/LSP authority.

The first three groups are **not automatically requirements** for this repo:
ADR 0023 explicitly keeps session latent memory and the full SCM agent-runtime
product out of this program, while hosted tenancy/cloud remains deferred. The
ranking, authority, security, durability, and operator-contract gaps remain
relevant to the stated local-first code-memory product.

## Competitor comparison

- **Aider:** its official repo-map docs and implementation use symbol extraction,
  dependency graph ranking, PageRank, relevance mentions, caching, and a hard
  token budget. Current contextpack matches the budget discipline but lacks a
  task-personalized code repo map and code-lane PageRank.
  Sources: [Aider repo map](https://aider.chat/docs/repomap.html),
  [Aider implementation](https://github.com/Aider-AI/aider/blob/main/aider/repomap.py).
- **Continue:** its retrieval pipeline combines multiple retrieval sources,
  deduplicates, then reranks a bounded candidate set with fallback behavior.
  Current code search has no dense/rerank lane, although hosted/local model
  clients exist for the memory path.
  Sources: [Continue retrieval](https://github.com/continuedev/continue/blob/main/core/context/retrieval/retrieval.ts),
  [Continue reranker pipeline](https://github.com/continuedev/continue/blob/main/core/context/retrieval/pipelines/RerankerRetrievalPipeline.ts).
- **ast-grep MCP:** exposes structural code matching, relational rules, and
  syntax-tree inspection. Current operator search has no structural rule lane.
  Source: [ast-grep MCP](https://github.com/ast-grep/ast-grep-mcp).
- **Serena:** uses LSP-backed semantic symbol navigation, references,
  implementations, diagnostics, and editing across many languages. Current
  graph authority is Go AST plus lexical fallback and intentionally has no live
  LSP/SCIP lane.
  Sources: [Serena](https://github.com/oraios/serena),
  [language support](https://oraios.github.io/serena/01-about/020_programming-languages.html).
- **Sourcegraph/SCIP:** precise navigation is represented as compiler-produced
  language-agnostic indexes with definitions, references, and implementations;
  search fallback remains available when precise data is absent. Current exact
  projections are deterministic but not SCIP-compatible.
  Sources: [precise navigation](https://sourcegraph.com/docs/code-navigation/precise-code-navigation),
  [SCIP](https://github.com/sourcegraph/scip).
- **GitHub Copilot:** repository/workspace semantic indexing complements text,
  grep, file, and usage tools. Current code lane has lexical/exact search only.
  Source: [repository indexing](https://docs.github.com/en/copilot/concepts/context/repository-indexing).
- **Graphiti:** temporal context graphs with provenance and historical querying
  match the direction of current bi-temporal claims, but current memory is not
  exposed through the full typed agent-memory contract.
  Source: [Graphiti](https://github.com/getzep/graphiti).
- **Letta:** always-visible mutable memory blocks plus searchable archival memory
  are a useful adapter/UX comparison; current Go cortex has tiers and recall
  primitives but no equivalent canonical agent-facing memory tools.
  Sources: [memory blocks](https://docs.letta.com/v1-sdk/memory/memory-blocks),
  [context hierarchy](https://docs.letta.com/guides/core-concepts/memory/context-hierarchy/).

## Recommended implementation order

1. **P0 security and honesty:** HTTP/MCP trust policy; ignore/index-gated
   `code_read`; repair broken docs and stale catalog/planned comments; add a
   contract test that every advertised alias/field is reachable and typed.
2. **P1 retrieval authority:** exact-identifier guarantee and ranking penalties;
   code-lane dense/rerank interface; repo map; durable fsync/lock/root binding;
   Tree-sitter/SCIP ingestion for TS/Python/Rust.
3. **P1 change safety:** canonical `code_apply_changeset` staging/verify/
   rollback/compare-and-swap engine, keeping the current pure validator.
4. **P2 agent ergonomics:** explicit query mode/budget parameters, diagnostics,
   structural search, typed memory/session adapters, and a lifecycle/install
   contract if this binary is intended to replace the previous CLI.
5. **P2 evidence:** repeatable code QA/retrieval benchmark and multi-client
   smoke matrix; promote only measured claims.
6. **P3 optional/deferred:** hosted tenancy, cloud sync/overlays, billing,
   public distribution, enterprise isolation, and full session product.

## Source inventory

> **Out-of-extraction classification.** Entries under “Local prior” / “Local
> prior rebrand” point at the pre-extraction Ouroboros/Sentra trees and are
> historical provenance only — not resolvable from this repo. Links into
> `docs/plans/`, `docs/stages/`, and `docs/reference/` in the decisions, specs,
> and findings under `docs/` are likewise artifacts of the pre-extraction
> source tree and are intentionally not vendored here.

- Local prior: `/Users/sammy/Developer/Sentra_Research/sentra-code-memory`
  (`docs/architecture/PARITY-MATRIX-v2.md`, `docs/ROADMAP.md`,
  `.claude/v1-v2-parity-audit.md`, `packages/mcp-server/src/tools/`).
- Local prior rebrand: `/Users/sammy/Developer/Sentra/sentra-code-memory` and
  `/Users/sammy/Developer/Sentra/code-memory-tokoyo`.
- Current repo: `services/brain/internal/codeserve`, `codecrawl`, `codeindex`,
  `contextpack`, `sessionlog`, `savings`, `workflow`, and
  `docs/specs/code-intelligence/README.md`.
- Current repo's historical SCM snapshot:
  `docs/research/2026-07-25-codecrawl-scm-parity.md` and
  `docs/research/2026-07-29-audit-fix-v5-parity-latency.md`.
