# Next steps

Status: post-P6 local parity backlog complete (2026-08-13).

## Delivered

Issues #41–#48 and follow-up #53 are closed and landed on `main`. The local
operator ships indexed search/read/exact/imports, bounded watch, ranked
retrieval, repo maps, structural and diagnostics surfaces, transactional
ChangeSets, typed local memory/session/savings surfaces, and deterministic
CLI/JSONL/HTTP/MCP smoke and retrieval benchmarks.

## Remaining by intent

### Product decisions, not defects

- Dense code embeddings and cross-encoder reranking remain deferred
  (`code_dense_rerank`).
- Full compiler/LSP and broad Tree-sitter authority remains deferred; SCIP
  documents can be ingested explicitly, while the default graph remains honest
  about heuristic authority outside Go.
- Lifecycle/install/server management and the full SCM session product remain
  outside the standalone binary.
- Hosted tenancy, cloud sync/overlays, billing, public distribution, and
  enterprise isolation remain non-goals.

See `docs/roadmap/DEFERRED-AND-NON-GOALS.md` and
`docs/decisions/0025-memory-session-lifecycle-parity.md`.

### Hygiene opportunities

- Keep the code-intelligence specification status aligned with the shipped
  bounded ChangeSet implementation; the broader compiler-grade contract is
  still a design target.
- Remove or reword the unused `StatusPlanned` catalog enum/comments.
- Audit historical research links that point into the extracted source tree
  (`plans/`, `stages/`, `reference/`, and sibling research artifacts).
- Add per-module manifests/build/test/deployment contracts if the advisory
  monorepo check is made a release requirement; current repo-native `just ci`
  remains green.
- Promote the benchmark from the checked-in ten-query heuristic gate to a
  broader measured corpus before making quality claims about other lanes.

These are intentionally follow-up work, not blockers for the local-first
operator release.
