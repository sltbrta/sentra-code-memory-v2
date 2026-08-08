# Projection propagation SLOs

Status for issue #316: **[planned targets, offline verifier and deterministic
drills, fail-closed query provider boundary]**. The targets below are v1 commitments, not
achieved product claims
(same posture as the [performance delivery contract](../performance/README.md)). The
verification helpers are offline-only: they consume receipt-backed samples and
never touch live infrastructure.

Machine-readable sources are `DefaultSLOs()` and
`DefaultReceiptSLOs(sourceID)` in `services/brain/internal/projections`. The
base table is locked by `TestDefaultSLOsMatchSpec`; the practical receipt matrix
is locked by `TestDefaultReceiptSLOsCoverEverySourceSurfaceOperation`.

## Scope and invariants

Projections are derived, rebuildable artifacts, never authority (ADR 0002 /
ADR 0020). Three enforcement facts are **invariants**, not SLOs, and are
already tested where they live:

- **Deletion denies immediately at authority** (OURO-SEC-008): a tombstoned
  evidence record is unreadable the instant the tombstone commits
  (`services/brain/internal/localstorage/evidence.go`).
- **Permission changes are enforced at read time** (OURO-SEC-010): every query
  reauthorizes at the query, hydrate, and emit checkpoints with the current
  ACL epoch; denial can never widen evidence
  (`services/brain/internal/query/engine.go`).
- **Projection absence is a coverage fact, never deletion evidence**
  (`services/brain/internal/query/ports.go`).

The SLOs bound what remains: **propagation lag** — how long canonical events
take to be verifiably reflected by each projection.

## Content-privacy admission before projection

Issue #318 adds a separate pre-projection invariant in
`services/brain/internal/contentprivacy`: raw extracted text must pass
`Guard.Admit` before a query-facing content, index, cache, claim, or citation
surface is publishable. `ProductionProjectionAdapter` is the explicitly named
composition seam. Construction requires a non-nil guard and publisher; a nil,
zero, detector-failing, quarantined, or tombstoned path cannot call the
publisher. Only the sanitized `Projection` prepared by the guard crosses the
sink port, and the admission becomes readable and receipt-bearing only after
the publisher succeeds. A failed or panicking publisher rolls back its exact
transient reservation so a retry can reuse the stable scoped identity. This
direction leaves canonical artifact and ACL authorities
outside the bounded library and adds no dependency from them to a derived
projection implementation.

The repository does not currently bind this seam to a deployed policy or
hosted sink, so this is a tested composition contract rather than a production
coverage claim. A deployment must additionally persist content-free policy
receipts and tombstones, keep originals inside its authorized encrypted vault,
and retain the current ACL/scope checks described below.

`contentprivacy.Evaluate` supplies deterministic offline precision, recall,
false-redaction, strict detector-class coverage, deletion correctness, and
citation-to-redacted-span measurements. Labels exist only in the offline
`EvaluationCase` type, never runtime `Input`, projections, or receipts. The
citation-to-redacted-span invariant is zero; zero retained citations remain
`0/0`, not claimed coverage. Full definitions and denominator conventions are
locked in the package README and tests. These fixture metrics are not live
telemetry or a universal detector-quality claim.

## Dimensions

| Propagation | EventAt (canonical) | ReflectedAt (projection) |
|---|---|---|
| `freshness` | complete generation published | projection serves that generation |
| `deletion` | tombstone appended | residual derived data purged from the projection |
| `permission_change` | ACL epoch bumped | no read path can still emit under the prior epoch |

## v1 targets per projection

| Projection | Propagation | p50 | p95 |
|---|---|---|---|
| `lexical` | `freshness` | 10 s | 60 s |
| `lexical` | `deletion` | — | 60 s |
| `lexical` | `permission_change` | — | 5 s |
| `ontology` | `freshness` | 5 m | 20 m |
| `ontology` | `deletion` | — | 20 m |
| `ontology` | `permission_change` | — | 5 s |
| `dense` | `freshness` | 5 m | 20 m |
| `dense` | `deletion` | — | 20 m |
| `dense` | `permission_change` | — | 5 s |

Rationale: lexical freshness aligns with `cold_exact_lexical_ready` (≤ 60 s)
and ontology/dense freshness with `semantic_graph_ready` (≤ 20 m) in
[performance-targets.yaml](../../reference/performance-targets.yaml). Deletion
targets equal the freshness p95 of their projection because residual purge
rides the same atomic publication machinery. Permission targets bound the
longest in-flight read that could still emit under a superseded epoch; the
emit-checkpoint reauthorization makes this the query duration ceiling, well
above the hybrid retrieval p95 (750 ms).

A p50 of `—` means only the tail is bound; the verifier treats a zero
`TargetP50` as disabled.

## Practical source receipt contract

`DefaultReceiptSLOs(sourceID)` expands the base budgets into four separately
reported operations (`index`, `update`, `delete`, and `permission_change`) for
each answer-path surface:

| Surface | Inherited budget profile |
|---|---|
| `lexical` | lexical |
| `dense` | dense |
| `graph` | ontology |
| `claims` | ontology |
| `cache` | lexical |
| `answer` | lexical |

Index and update inherit the profile's freshness p50/p95. Delete and permission
change inherit its deletion and permission targets. The inheritance is a
measurement rule, not a claim that all six surfaces are independent stores.
Claims and answer are included because stale support can survive retrieval in a
materialized claim, cached result, or emitted answer even after the underlying
index has caught up.

Every `PropagationReceipt` is source-specific and non-payload-bearing. It pins
an event ID, operation, surface, generation ID, current generation ID,
`GenerationAt`, `TombstoneAt` when deleting, `PermissionChangedAt` and ACL epoch
when permissions change, `ReflectedAt`, attempt, success, and exact tombstone
completion. `ReceiptID` is globally unique within a drill input. A receipt is
inadmissible if its canonical timestamps differ from the event, its worker
generation is no longer current, it moves time backward, it reports an
incomplete tombstone, its permission ACL epoch is not exactly the event epoch,
or a non-permission receipt carries a non-zero ACL epoch. Retries are counted
but a canonical event contributes only its earliest valid
success to percentiles, so retries cannot improve or inflate the distribution.

`RunPropagationDrill` deterministically joins canonical fixture events to
receipts and reports source/surface/operation p50 and p95, missing events,
rejected receipts, retries, and tombstone completeness. Every expected delete
must have a complete receipt on all six surfaces: compliance requires exact
`Complete == Expected` (reported as 100%), never a rounded approximation.
Fixtures cover initial index, update, delete, ACL revocation, failed then
successful retry, stale-worker completion, and one-surface partial failure.

`AdmitEvidence` is the offline receipt-admission decision helper. It requires the
latest generation and ACL event to have an exact, within-p95 receipt on every
required surface at the observation time. It denies a superseded generation,
missing/late/failed surface, or stale ACL epoch. A canonical tombstone denies
immediately even if purge receipts are incomplete.

The query engine requires an `EvidenceAdmitter` at construction unless a caller
selects the explicitly named legacy opt-out. Receipt admission runs at the final
emit checkpoint; after a successful admission the engine rechecks the canonical
generation and invokes admission again with a new observation time. A tombstone,
new generation, or ACL event committed while the first admission blocks must
therefore participate in the final decision. Denial emits no claims, citations,
or prose. The adversarial test snapshots before blocking, commits a newer
tombstone during the wait, and proves the post-admission recheck rejects it.

`localauthority.OpenQuerySurface` is the receipt-enforced composition boundary:
exactly one non-nil provider is required and absence fails at construction. The
retired local Stage 04 gateway has no live propagation-receipt source and uses
the explicitly named legacy constructor to preserve its frozen contract; it is
not an organization-brain rollout. Accordingly, this repository still makes no
claim that a live product receipt source or achieved propagation SLO ships.
Immediate deletion/current-generation/ACL safety on that retired path continues
to come from the authority invariants above.

Focused targets:

```sh
cd services && go test ./brain/internal/projections -count=1
cd services && go test ./brain/internal/query -run 'TestAnswer.*ReceiptAdmission' -count=1
bazel test //services/brain/internal/projections:projection_slo_receipts_test
```

## Verification contract (fail closed)

`Verify(slos, samples)` returns one measurement per SLO with admitted and
rejected sample counts, observed p50/p95 (nearest-rank), and a verdict:
`met`, `missed`, or `unverified`. `Compliant(measurements)` is true only when
every verdict is `met`.

A sample is **inadmissible** — counted as rejected and never able to prove
compliance — when any of the following holds:

- `EventAt` or `ReflectedAt` is zero, or `ReflectedAt` precedes `EventAt`;
- the backing receipt is not pinned (`GenerationID` or
  `CurrentGenerationID` empty);
- the receipt pin is **stale** (`GenerationID` ≠ `CurrentGenerationID`);
- the receipt evidence record is **tombstoned**. (Deletion samples measure a
  tombstoned *subject*; the measurement receipt itself must stay readable.)

Fail-closed consequences:

- zero admissible samples ⇒ `unverified`, and `unverified` is never
  compliant — absence of evidence is not compliance;
- an empty measurement set is never compliant;
- rejected samples are excluded from percentiles in both directions: they can
  neither prove nor mask a breach;
- malformed SLO definitions (unknown kinds, missing p95, p50 > p95,
  duplicate dimension) are rejected outright.

## Non-goals (v1)

- No live probes, exporters, dashboards, alerting, receipt datastore, or
  organization-brain receipt provider. Such a composition must supply current
  canonical events and receipts to the required query port; absence fails
  closed, and request fields alone cannot establish admission.
- No per-tenant target overrides. Source IDs partition receipt measurements;
  targets still inherit the frozen v1 projection profiles.
- No SLO for the conversation store or canonical authority itself — those are
  authority invariants, not projection propagation.
