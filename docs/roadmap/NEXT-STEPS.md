# Next steps

Status: post-P6 local parity backlog complete (2026-08-13); issue #54
hygiene follow-up (spec status, catalog terminology, link audit, benchmark
expansion) recorded below.

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

Resolved in the issue #54 follow-up (docs/benchmark/authority-evidence slice
of #55); the items below are closed or recorded as decisions:

- **Code-intelligence spec status aligned.** The spec now states
  `partially implemented`: the bounded transactional ChangeSet engine ships
  behind `code_apply_changeset`, while the broader compiler-grade target
  (LSP/SCIP authority, full blast-radius closure, full transaction flow)
  remains a design target. See `docs/specs/code-intelligence/README.md`.
- **`StatusPlanned` removed.** No planned verb remains; every catalogued verb
  is `stable` or `deferred`. The unused enum and its comments were removed and
  a conformance test locks the invariant.
- **Historical research links classified.** Out-of-extraction references to
  the pre-extraction Ouroboros/Sentra tree and to `plans/`/`stages/`/
  `reference/` artifacts are now explicitly marked as historical and
  non-resolvable in this repo (see the parity audit's source inventory note).
- **Per-module manifests decision recorded.** The repo-native `just ci`
  remains the release gate; `monorepo_check` stays advisory. Per-module
  manifests/build/test/deployment contracts are not a release requirement for
  this extraction. See `docs/roadmap/DEFERRED-AND-NON-GOALS.md`.
- **Benchmark broadened.** The deterministic offline gate grew from 10 to 24
  checked-in probes spanning every `qafixture` package (19 exact-identifier +
  5 lexical), with the baseline digest regenerated. Quality claims about the
  deferred dense/compiler lanes remain intentionally unmade.

These are now closed follow-up items, not blockers for the local-first
operator release.
