# orgscope — issues #311/#312 fail-closed and recovery contracts

Hermetic model and tests for tenant/scope isolation, SCIM-shaped lifecycle with
revocation receipts, and erasure tombstones across in-process projections,
in the companymode `FakePostgres`/`FakeS3` style.

```sh
go test ./services/brain/internal/orgscope/ -count=1
go test -race ./services/brain/internal/orgscope/ -count=1
go vet ./services/brain/internal/orgscope/
bazel test //services/brain/internal/orgscope:orgscope_test
```

## Surfaces

- `Directory` — SCIM-shaped user/group lifecycle (provision, deprovision,
  group create, member add/remove). Every mutation bumps the policy epoch and
  appends an append-only `Receipt` (policy revocation receipts).
- `Authority` — default-deny evaluator over `individual:<user>`,
  `team:<group>`, `company` scopes: deny overlays beat every allow (including
  the implicit own-individual allow), delegated grants die on expiry or when
  the delegator is offboarded, and every non-allow resolves to the single
  non-disclosing `ErrDenied`.
- `Store` — primary items plus local derived projections: search index, local
  claim search, source-keyed graph nodes, query cache, per-principal session
  history and replay artifacts, append-only audit (ids and digests only, never
  content; survives erasure). Every content-returning projection read
  re-filters through the current authority and tombstones, so cached, replayed,
  graphed, claimed, or rebuilt artifacts cannot disclose revoked or erased
  memory. Principal-keyed artifacts are also bound to the user's current
  lifecycle incarnation.
- `Erase` / `EraseScope` / `EraseOwner` / `VerifyErasure` — tombstones purge and
  verify the exact locally managed content-bearing projection set named by
  `LocalStoreErasureCoverage`; successful scope/owner erasure installs a
  permanent write fence until a higher-level policy owner reprovisions it.
  Audit identifiers/digests intentionally remain.
- `RebuildProjections` / `CreateBackup` / `Restore` — a same-tenant restore
  checksum-validates canonical contents, unions tombstones already present in
  the destination with those in the backup, and atomically rebuilds derived
  projections. Exported `Backup` values are caller-owned and are not erased by
  `Store.Erase`; the checksum is not an authenticity mechanism.
- `CreateRecoveryBackup` / `CreateACLSnapshot` / `RunRecoveryDrill` — reusable,
  disposable recovery contract for generation/config-pinned backups, bounded
  contiguous queue replay, current ACL restoration, projection rebuild, and
  exact tombstone/non-resurrection verification. Receipts include backup and
  ACL digests, queue cursors, RPO/RTO objectives and observations, ACL probes,
  tombstone completeness across the declared substrate matrix, and deterministic
  failure-injection points. `RecoveryDrillRunner` provides bounded immediate or
  scheduled execution with fsync-backed retention of success and failure
  receipts; production certification remains false.
- `ReportCard` → `ComplianceReport` — caller-recorded hermetic leak,
  stale-grant, local erasure, and restore/rebuild probe summaries plus explicit
  non-claims. It is not a full issue-acceptance or production certification.
  disposable recovery contract for a generation/config-pinned backup, a
  bounded contiguous queue replay, current ACL restoration, projection
  rebuild, and exact tombstone/non-resurrection verification. Receipts include
  backup and ACL digests, queue cursors, RPO/RTO objectives and observations,
  ACL probes, tombstone completeness across every content substrate represented
  here (including a canonical tombstone-manifest digest), all eight typed
  repository substrate classes, a representative recovered-target query, and
  deterministic failure-injection points.
- `RecoveryDrillRunner` / `FileRecoveryReceiptRetainer` — immediate or
  caller-scheduled one-shot execution under a context deadline, followed by
  immutable content-addressed JSON retention. Passed, rejected, injected,
  cancelled, timed-out, and not-yet-due results all produce retained negative
  or positive receipts. Failure to retain downgrades a pass to failed.
- `ReportCard` → `ComplianceReport` — leak rate, stale-grant rate, erasure
  completion rate, restore correctness, plus explicit substrate non-claims.

## Issue #312 bounded recovery drill

`RunRecoveryDrill` accepts only an empty disposable target. Before mutation it
verifies the tenant, generation, config digest, backup digest, current ACL
snapshot digest, queue generation/sequence/time continuity, declared queue
bound, and RPO. It then restores the backup, replays `item.put` / `item.erase`
events, restores the current lifecycle/grant/deny/receipt state, runs behavioral
ACL probes, rebuilds projections, runs a representative authorized query that
must populate non-empty recovered cache and session entries, and checks the
complete expected tombstone set against:

- primary items;
- search index;
- query cache;
- session history;
- a post-recovery backup manifest;
- restoration of the older backup under the current tombstone overlay; and
- re-ingest rejection for tombstoned backup items.

The request must also bind exactly one adapter for each typed substrate kind:

| Required kind | Repository examples represented by the kind |
| --- | --- |
| `filesystem` | local brain/projection files |
| `sql` | SQLite/Neon/PostgreSQL rows and ledgers |
| `vector` | local dense, pgvector/Qdrant/FAISS projections |
| `hotlex` | HotLex BM25 serving projection |
| `graph` | ontology/relation graph projections |
| `claims` | claim/evidence projections |
| `cache` | query/entity/embed caches and recovered sessions |
| `object` | S3-compatible blob/object storage |

`NewHermeticRecoverySubstrateMatrix` supplies deterministic identifier-only
fake adapters for all eight. Missing, nil, duplicate, unknown, unavailable, or
inconclusive adapters fail closed. The fake restore requires an empty adapter,
starts from pre-erasure backup candidates plus current live identifiers,
applies the tombstone overlay, and verifies every expected erased identifier
is absent while representative live identifiers remain. This is explicit
fixture coverage, not a claim that the corresponding provider was contacted.

For scheduled operation, an external cron/queue invokes
`RecoveryDrillRunner.Run` with `RecoveryDrillJob.ScheduledAt`; the package does
not start an immortal scheduler goroutine. Runs default to the 60-minute RTO
bound (configuration above 24 hours is rejected), adapters must honor context
cancellation, and receipt persistence has its own 30-second bound. The file
retainer uses private, fsync'd, immutable JSON files and never automatically
deletes negative receipts; directory lifecycle/retention policy remains an
operator responsibility.

The receipt remains failed and `verified=false` for malformed/tampered pins,
queue gaps or generation rollback, ACL mismatch, incomplete/extra tombstones,
missing/inconclusive substrates, empty representative queries, resurrection
attempts, RPO/RTO misses, occupied targets, schedule/deadline failures, receipt
retention failures, and every injected failure point. `production_certified`
is always false.

## PR review follow-up hardening (issue #311)

- User, group, and delegated grants are bound to lifecycle incarnations:
  offboarding/deleting permanently invalidates grants issued to or delegated
  by that incarnation, and recreating the same external id cannot resurrect
  them.
- Concurrent revocation wins: `Resolve`, `Query`, `History`, `Replay`, graph,
  and claim reads only serve a decision made under a stable policy epoch. A
  mid-flight offboarding,
  revocation, or deny overlay forces re-evaluation against the mutated
  policy; an unsettled policy denies fail-closed
  (`TestOffboardingWinsInFlightReads`, `TestConcurrentReadsRevocationAndErasureRace`).
- Integrity-checked restore: `Restore` recomputes the unkeyed SHA-256 checksum
  over canonical (id-sorted) backup contents and rejects modification unless
  the checksum is recomputed. The checksum is not a signature or provenance
  proof (`TestRestoreRejectsModifiedBackupWithoutMatchingChecksum`).
- Immutable audit receipts: `Audit()` returns deep copies; mutating returned
  entries or receipt id slices never alters the retained log
  (`TestAuditEntriesAreDeepCopies`).
- In-flight erase wins: `Query`/`History` revalidate item and tombstone state
  after `Resolve` and before returning snippets, and never re-seed cache or
  session projections with tombstoned ids
  (`TestInFlightEraseFailsClosedInQuery`, `TestInFlightEraseFailsClosedInHistory`).

## Issue #311 hermetic evidence (this slice)

“Exercised” below means this package's in-process model has a passing test. It
does not mean the corresponding production acceptance item is complete.

| Acceptance item | Status |
| --- | --- |
| Default-deny scope isolation | exercised (`TestDefaultDenyScopeIsolation`) |
| Group/role changes | exercised (`TestGroupJoinRoleChangeOffboardingAndReceipts`) |
| Delegated grants | exercised (`TestDelegatedGrantExpiryAndDelegatorOffboarding`) |
| Deny overlays | exercised (`TestDenyOverlayBeatsEveryAllow`) |
| Session history / caches / indexes | exercised via read-time re-filter, lifecycle binding, and local erasure purge |
| Graph, claims projections | local source-keyed projections exercised; production integration not certified |
| Backups / replay artifacts | same-store tombstone union and post-erasure fresh restore exercised; caller-owned backup deletion and an external erasure ledger are not modeled |
| SCIM lifecycle + revocation receipts | in-process contract exercised; **no HTTP SCIM endpoint** |
| Erasure across projections, no reappearance | local content projections exercised (`TestErasureAcrossProjectionsRestoreAndRebuild`) |
| Red-team zero unauthorized citations | observed for the finite local probe bank (`TestRedTeamZeroUnauthorizedCitationsReport`) |
| Erasure SLO / leak / stale-grant / restore report | emits local sample counts/rates and p95; no production SLO or full-acceptance claim |

## Non-claims and production-provider limits

No live PostgreSQL RLS, OpenFGA cloud tuples, network SCIM endpoint, KMS
crypto-shred, production memory/ontology graph or claim integration, backup
artifact deletion, external erasure ledger, backup/restore/rebuild operator
authorization, audit-metadata deletion, production erasure SLO, PostgreSQL
PITR/WAL, S3 version inventory, OpenFGA tuple export, production queue
infrastructure, regional failover, writer fencing, live-provider RPO/RTO, or
multi-region backup certification. `Backup.Digest` is not an authenticity
mechanism. The #312 drill uses only hermetic in-process adapters and its RPO/RTO
values are contract calculations from caller-pinned instants, not production
measurements. Those remain open under #311/#312 (parent #306, gate #307) and
DEF-015 — see
[DEFERRED-AND-NON-GOALS](../../../../docs/roadmap/DEFERRED-AND-NON-GOALS.md).

## Issue #312 matrix evidence

| Acceptance item | Status |
| --- | --- |
| Default-deny scope isolation | proven (`TestDefaultDenyScopeIsolation`) |
| Group/role changes | proven (`TestGroupJoinRoleChangeOffboardingAndReceipts`) |
| Delegated grants | proven (`TestDelegatedGrantExpiryAndDelegatorOffboarding`) |
| Deny overlays | proven (`TestDenyOverlayBeatsEveryAllow`) |
| Session history / caches / indexes | proven via read-time re-filter + erasure purge |
| FS, SQL, vector, HotLex, graph, claims, cache, object matrix | hermetic typed adapters proven; live provider wiring explicitly open |
| Backups / replay artifacts | proven for this store's backup/restore/rebuild |
| SCIM lifecycle + revocation receipts | in-process contract proven; **no HTTP SCIM endpoint** |
| Erasure across projections, no reappearance | proven (`TestErasureAcrossProjectionsRestoreAndRebuild`) |
| Red-team zero unauthorized citations | proven (`TestRedTeamZeroUnauthorizedCitationsReport`) |
| Erasure SLO / leak / stale-grant / restore report | `ComplianceReport` emitted and asserted |

## Issue #312 non-claims and production-provider limits

No live filesystem inventory, PostgreSQL RLS, vector service, HotLex volume,
OpenFGA cloud tuples, network SCIM endpoint, KMS crypto-shred, production
graph/claims/cache/object erasure, or multi-region backup certification. Those
remain open under #311 (parent #306, gate #307) and
DEF-015 — see [DEFERRED-AND-NON-GOALS](../../../../docs/roadmap/DEFERRED-AND-NON-GOALS.md).

The built-in #312 matrix uses only this package's hermetic in-process store,
directory, authority, queue-entry contract, and deterministic identifier-only
adapters. Every substrate receipt says `provider_boundary=hermetic_fake` (or
`provider_adapter` only when a caller explicitly supplies one), while
`production_certified` remains false in both cases. Its RPO/RTO values are
contract calculations from caller-pinned instants; they are not measurements
of PostgreSQL PITR/WAL, S3 version inventory, OpenFGA tuple export, KMS key
availability/crypto-erasure, production queue infrastructure, regional
failover, writer fencing, or live provider latency. Passing it is unit-level
evidence and must not be presented as live disaster-recovery certification.
