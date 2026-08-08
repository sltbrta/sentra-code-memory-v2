# Stage 05 factory kernel

Status: **[shipped] kernel leaf; no runner, gateway, or TUI surface.** This
package is the deterministic Stage 05 factory kernel: ChangeIntent admission,
typed one-layer DAG compilation and validation, durable leases/fences,
model-routing facts, candidate atomicity, and the typed review reducer over
migration 005. It registers no handlers, opens no sockets, and keeps every
prose or large payload — goal text, admitted intent bytes, candidate preview
bytes, message payloads, finding summaries — inside the encrypted
ArtifactVault behind the `PayloadStore` port. The gateway leaf composes it
behind the frozen five-RPC `FactoryService`; this package only owns kernel
behavior.

## API

```go
kernel, err := factory.Open(ctx, factory.Config{...})
admitted, err := kernel.AdmitChangeIntent(ctx, factory.AdmitRequest{...})  // run + typed plan, atomic
plan, err := kernel.GetChangePlan(ctx, identity, runID)                    // served frozen ChangePlan
preview, err := kernel.PreviewChangeSet(ctx, identity, runID)              // served candidate preview
page, err := kernel.GetReviewFindings(ctx, identity, runID, cursor, 50)    // typed findings page
cancelled, err := kernel.CancelChangeRun(ctx, factory.CancelRequest{...})  // terminal CANCELLED
err = kernel.TransitionRun(ctx, identity, runID, next)                     // bounded lifecycle
result, err := kernel.CommitLeafResult(ctx, identity, runID, node, fence, bytes)
err = kernel.RecordGateResult(ctx, identity, runID, gateID, status)
err = kernel.ProposeCandidate(ctx, identity, runID, preview)
err = kernel.TransitionCandidate(ctx, identity, runID, next, rollback)
finding, err := kernel.RecordFinding(ctx, identity, runID, draft)
err = kernel.DisposeFinding(ctx, identity, runID, findingID, disposition)
sent, err := kernel.SendMailboxMessage(ctx, identity, runID, input)
pending, err := kernel.PendingMailboxMessages(ctx, identity, runID, taskID)
replayed, err := kernel.AcknowledgeMailboxMessage(ctx, identity, runID, messageID)
```

- `Open` attaches to an already-migrated authority database (migration 005
  verified, fail-closed `ErrSchemaUnsupported`). The composing local
  authority owns migrations and the process owner lock; the kernel takes
  neither.
- All three reads (`GetChangePlan`, `PreviewChangeSet`, `GetReviewFindings`)
  resolve runs under the authenticated tenant and principal scope; unknown,
  cross-principal, and revoked (cancelled) runs share the static
  `ErrNotFoundOrDenied`, matching the frozen read boundary.
- `AdmitChangeIntent` is authorization-first: caller cross-check against the
  authenticated session, current-policy check, approval revalidation
  (present, receipt-completed, unexpired), and exact Git base resolution
  against the Stage 03 ingestion authority all pass before any canonical
  fact commits. Every denial — unknown, unauthorized, stale base, stale
  approval, conflicting idempotency reuse — shares the static
  `ErrNotFoundOrDenied`, so the gateway maps all of them to the frozen
  non-disclosing shape. An exact authenticated replay returns the original
  run with `Replayed`. The bounded slice turns exactly one approved intent
  into one run: a second admission of the same intent under a different key
  denies statically rather than opening a duplicate DAG.
- The plan compiler builds the frozen one-layer DAG (one orchestrator, one
  to three leaves, at most one review node, four required gates) and
  validates it twice: Go checks mirroring every frozen CEL rule (unique node
  identifiers, prefix-disjoint leaf scopes attenuating the approved intent
  scope, normalized paths, leaf-only scope/lease/grant/route, no dispatch
  authority), then `protovalidate` on the compiled `ChangePlan` as
  defense-in-depth. Violations are `ErrPlanInvalid`. Scope disjointness and
  attenuation fold ASCII case because the shipped platform's default
  filesystem (APFS) is case-insensitive and candidates materialize on disk;
  the served plan preserves the declared case, and holder principals are
  validated as printable bounded identities before any payload staging.
- Leases issue densely fenced per leaf (see `factory/roster`); commits under
  an expired or superseded fence are `roster.ErrStaleFence` and never become
  canonical. Leaf results commit exactly once per leaf; an exact replay
  collapses on the canonical digest, a differing result conflicts.
- Candidates are all-or-nothing: `ProposeCandidate` enforces the frozen
  preview shape (unique post-image and pre-image paths, per-language
  obligations, unique gate identities, exact base binding) and rejects any
  edit outside every leaf's owned scope as `ErrScopeEscape`. `VERIFIED`,
  `REVIEWED`, and `RETAINED` require every required gate passed; `REJECTED`
  requires the rollback receipt in the same atomic commit. `COMPLETED` runs
  require a retained candidate.
- The review reducer (`RecordFinding`/`DisposeFinding`) enforces the typed
  vocabularies, reviewer disjointness from the admitting principal (every
  leaf grant's initiator), evidence-required dismissal, and exactly-once
  dispositions; open findings never carry a disposition receipt.
- Mailbox operations wrap the durable ledger (see `factory/mailbox`) with
  run-scope and liveness checks; duplicate delivery collapses to the
  original dense per-task sequence with the canonical digest as the
  authoritative replay match. Every static denial and exact replay resolves
  before payload staging, so denied attempts never leave unreferenced
  vault objects.
- Model routing is deterministic through the `Router` port; `StaticRouter`
  pins one certified profile so route decisions replay exactly in tests.

## Errors and the static boundary

`ErrInvalidInput` is a programming error at the boundary. `ErrNotFoundOrDenied`
is the single non-disclosing denial. `ErrPlanInvalid`, `ErrScopeEscape`,
`ErrTransitionInvalid`, and `ErrReviewerConflict` name reducer rejections;
`roster.ErrStaleFence` and `roster.ErrResultConflict` name lease and result
rejections; `mailbox.ErrMessageConflict` and `mailbox.ErrUnknownMessage` name
message rejections. None of them carry upstream text, so the gateway can map
every one to the frozen static response without leaking existence detail.

Run `bazel test //services/brain/internal/factory:factory_test`.
