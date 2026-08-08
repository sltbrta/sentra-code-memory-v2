# Issue #276 — Parallelize safe post-retrieval hydration and structure work

## Problem

`retrieveInteractive` runs the post-Phase-A hydration chain (hydrate-by-id →
offline entity stubs → sibling hydrate) strictly serially before the structure
chain (path2 SQL → project hop2 → path2 doc hydrate; temporal relations). Both
chains are Neon-bound with independent budgets, so their walls stack on the
critical path even though each only needs the fused RRF pool: the hydrate chain
needs chunk/doc IDs, and the structure chain needs seed doc IDs plus durable
path2/cortex structure. The residual path in `retrieve.go` already overlaps
sibling hydrate with path2 structure under per-arm timers; interactive did not.

## Contract

- Only independent arms overlap:
  - **Arm H (hydrate chain)** stays internally serial — hydrate-by-id →
    entity-stub hydrate → sibling hydrate — because each step consumes the
    previous step's pool state (filled texts change stub classification).
  - **Arm S (structure chain)** runs path2 SQL, the dependent project second
    hop (seeded by first-hop docs), and path2 doc hydrate. Dependent hops inside
    the arm remain ordered. Temporal-relation left-shift runs after the join
    because its in-process helper has no cancellation surface.
- Arms never share mutable state. Arm H owns a private copy of the fused pool;
  Arm S reads an immutable seed snapshot and question-scoped cortex state.
  Diagnostics are collected per arm into arm-local maps and merged by the
  parent after the join, so the shared `diag` map is written by one goroutine
  only.
- Path2 seed doc IDs are derived from the pool top-6. In the common stub-free
  case, the pre-hydrate snapshot is stable and Arm H and Arm S overlap directly.
  When offline entity stubs are present, the seed-affecting hydrate prefix
  (hydrate-by-id → entity-stub hydrate) runs first and compares the exact,
  ordered `path2Seeds` sequence before and after hydration. If the sequence is
  unchanged, only the remaining sibling hydrate overlaps Arm S and the
  diagnostic `entity_stub_seed_ids_unchanged=true` is emitted. If it changes,
  the section falls back to fully serial legacy ordering and reports
  `hydrate_structure_serial_reason=offline_entity_stub_seed_safety`. The
  `OUROBOROS_ERB_SERIAL_HYDRATE_STRUCTURE=1` switch always restores that fully
  serial ordering. Neither max_inputs nor ranking change — execution shape is
  selected only after seed equivalence is established.
- Post-join work stays serial and order-preserving: pool-virtual structure
  expansion and the facts channel run on the hydrated pool exactly as before,
  then merges happen in the established order (structure neighbors → facts),
  followed by corpus grep, cross-encoder rerank, coverage/authority/supersession
  window passes, recency pack, and best-last. Ranking, scoring, and final
  evidence ordering are unchanged.
- Every arm keeps its existing per-step budgets (`hydrateBudget`,
  `structureSQLBudget`, `structureHydrateBudget`, the 1.5s hydrate-by-id cap,
  the 2.5s path2 cap, the 2s project hop2 budget) derived from the caller
  context, so parent cancellation and deadlines cancel all SQL-bound work.
  Temporal-relation left-shift is invoked only after the join and is skipped
  when the caller context is already done; its helper has no cancellation
  surface, so it is deliberately not part of the overlapped arm budget. No
  budget is multiplied by parallelism; the stub-stable path is bounded by
  seed-prefix + max(remaining hydrate, structure), while the ordinary path is
  approximately max(arm), not sum.
- The post-synthesis size-limit rebind is gated by explicit size/storage/upload
  vocabulary (word-boundary matched). Generic questions containing only
  "limit" must bypass the full-pack size scan, preserving the existing
  dual-limit correction for upload/attachment questions without changing
  provider synthesis or citation policy.
- Authorization is unchanged: every hydration/path2 query stays BrainID-scoped
  (`brain_id = $1`); parallelism never crosses the tenant boundary, and ACL
  filtering downstream of the pool is untouched (#285 reuse semantics rely on
  this).

## Diagnostics

New keys, stamped alongside the existing per-stage keys:

| key | meaning |
| --- | --- |
| `hydrate_structure_parallel` | `true` only when both arms actually ran concurrently (false for `OUROBOROS_ERB_SERIAL_HYDRATE_STRUCTURE=1`, an unstable offline-entity-stub seed-safety fallback, path2-ineligible serial mode, and the no-store synchronous path where there is no hydrate arm to overlap) |
| `entity_stub_seed_ids_unchanged` | emitted when the entity-stub seed prefix ran; `true` permits sibling-hydrate/structure overlap, `false` forces the serial seed-safety fallback |
| `hydrate_structure_serial_reason` | set when the section ran serially for a non-env reason: `offline_entity_stub_seed_safety` or `path2_ineligible` |
| `hydrate_structure_wall_ms` | wall time of the whole overlapped section |
| `hydrate_arm_ms` | arm H wall (hydrate-by-id + entity stubs + sibling) |
| `structure_arm_ms` | arm S wall (path2 chain; temporal is post-join) |

Existing keys keep their meanings: `hydrate_by_id_ms/_n/_error`,
`entity_stub_hydrate_*`, `hydrate_ms`, `hydrate_policy`, `structure_ms`,
`structure_sql_budget_ms`, path2/hop2 keys, temporal and facts keys. Failures
stay attributable per stage (`hydrate_by_id_error`, `path2_*_error`,
`path2_structure_context_status`), so wall vs arm time vs failures are
separable in G15 p95 triage. `latency_breakdown` gains the three arm/wall keys.

## Out of scope

- Ranking/scoring changes, provider answer synthesis, Modal URL, `max_inputs`,
  and #292 protocol behavior.
- Residual path (`retrieve.go`) and local interactive path changes.
- New budget knobs; existing env overrides remain the only tuning surface.

## Verification

Focused tests prove: parallel wall < sum of arm walls with per-arm diagnostics
present (arms pay genuinely different store latencies); deterministic window
ordering across repeated runs under jittered driver latency; serial-mode
equivalence on the full passage contract (text/score/channel/locator, not just
identity) on a stub-free pool; stable offline-entity seeds overlap only after the
exact seed sequence comparison and match env-forced serial on the full contract;
the unstable offline-entity-stub seed-safety fallback (runs serially, matches
env-forced serial on the full contract, reason stamped);
projectish hop2 ordering determinism (second hop surfaces fresh peers); the
no-store path reports `hydrate_structure_parallel=false`; per-arm deadline
bounding when the store stalls past budget; pre-canceled context short-circuit;
and BrainID scoping plus hydrate/path2 shape presence and query-count checks on
every query issued by both arms. Race-clean under `-race`. Full hosted + brain
suites and `just ci` stay green.

## Serial-mode ordering claim (review honesty)

`OUROBOROS_ERB_SERIAL_HYDRATE_STRUCTURE=1` (and the seed-safety fallback) run
the faithful legacy serial execution: hydration completes first, path2 seeds
are derived from the hydrated, poolK-trimmed pool exactly as the pre-#276
pipeline did, and the post-join pool-virtual expansion merges pool-virtual →
path2 → temporal in legacy order on that same hydrated pool. The wall-clock
grouping inside the structure section differs from legacy (path2 now runs
before pool-virtual expansion rather than after), but `structureExpandPassages`
reads only the hydrated pool and the merge order is unchanged, so `structNeigh`
and the final window are identical to legacy serial ordering — not merely
"a safe serial execution." The deterministic-ordering and serial-equivalence
tests pin this on the full passage contract.

## Evidence status (warm latency / pinned quality)

The issue's acceptance criteria call for measurable warm-latency improvement on
multi-document/project/completeness cases and no retrieval-quality/citation
regression on the pinned type matrix. Those require real Neon receipts against
the pinned matrix and are **pending offline verification** — the hermetic test
store proves wall < sum-of-arms and deterministic equivalence, not production
warm-latency or pinned-matrix quality. No quality or warm-latency improvement
is claimed without a real pinned receipt.

## Low-confidence seed-dense force-expand

When `shouldSignalAgentic` returns `low_confidence` and `nSeedDocs >= 12`, the
previous gate declined to escalate the request because the seed pool was dense
(`signal_agentic_suppressed = "seed_dense_low_confidence"`). Dense seed pools
with weak CE scores can still miss relevant documents that reformulation may recover.
The gate now forces at least one bounded ExpandLite round via
`AgenticOptions.ForceExpand`; the existing profile/override round cap remains
in force:

- `agentOn = true`, `forceExpand = true`, diagnostic reason `forced_low_confidence`
- `forceOne` is set inside `agenticExpand`, bypassing the local nSeed/gap skip
  for the first round
- ACL, filter, and source scope are preserved; GoldDocIDs are never passed
- `Enabled=false` kill switch still hard-off (checked before ForceExpand)
- Aggregation heuristic and explicit agentic-round profiles are unchanged
