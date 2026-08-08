# Bounded-factory gateway handlers

Status: **[shipped] handlers with boundary tests; unmounted until the Stage 05
kernel leaf (#135) and integration wiring land.** This package implements the
five frozen Stage 05 `FactoryService` RPCs — `AdmitChangeIntent`,
`GetChangePlan`, `PreviewChangeSet`, `GetReviewFindings`, and
`CancelChangeRun` — behind the authenticated owner-only gateway boundary,
mirroring the Stage 03/04 conventions exactly. Nothing here is registered on
the gateway socket; composition is integration scope, so the factory surface
stays fail-closed until the kernel and the mount land.

## API

```go
handler, err := factoryapi.NewHandler(factoryapi.Config{
    Kernel: kernel, Clock: clock, ConfigurationDigest: digest,
})
admit, err := handler.AdmitChangeIntent(ctx, peer, request)   // AdmitChangeIntentRequest  -> AdmitChangeIntentResponse
plan, err := handler.GetChangePlan(ctx, peer, request)        // GetChangePlanRequest      -> GetChangePlanResponse
preview, err := handler.PreviewChangeSet(ctx, peer, request)  // PreviewChangeSetRequest   -> PreviewChangeSetResponse
findings, err := handler.GetReviewFindings(ctx, peer, request) // GetReviewFindingsRequest  -> GetReviewFindingsResponse
cancel, err := handler.CancelChangeRun(ctx, peer, request)    // CancelChangeRunRequest    -> CancelChangeRunResponse
```

Each method takes the authenticated `localauthority.PeerContext` separately
from the complete decoded request, mirroring the Stage 04 `queryapi` port
shape, so the composing server can mount the methods on the frozen
`/ouroboros.contracts.v1.FactoryService/<Method>` procedures.

## Boundary rules

- **Validate, then identity, before any port.** Every method first executes
  the generated `buf.validate` field, required-oneof, and CEL rules on the
  decoded message, then cross-checks the untrusted body caller — principal,
  tenant, and both session references — against the authenticated peer. Both
  gates run strictly before any port invocation: validation failures return
  `ErrInvalidRequest`, and a mismatch or unmapped peer returns
  `ErrRequestDenied`, which the transport maps to its static `request-denied`
  shape. The trusted principal is always derived from the peer, never the
  body.
- **Every response revalidated.** Every constructed response — success and
  denial alike — is revalidated against the frozen descriptors before return;
  a contract violation (including any kernel-authored plan, preview, or
  finding that breaks the frozen CEL invariants) returns `ErrInvalidResponse`
  and fails closed. Kernel defects the contract cannot express fail closed
  the same way before construction: a missing admitted run identity on
  admission, and a cancellation echo that does not match the requested run
  (whose completed receipt binds the request identity, never the echo).
- **Static non-disclosure.** Unknown, unauthorized, stale, and revoked runs —
  and on writes a stale base, a stale lease or fence, a conflicting
  idempotency-key reuse, and a revoked grant — return the one static
  `not_found_or_denied` outcome with a rejected receipt and zero evidence
  refs, in-band as the frozen contract freezes. Port errors with no contract
  shape return `errPortFailure` and never carry upstream text.
- **Request-bound receipts.** Completed outcomes bind the admitted run
  identity; denials use the one static shape; every receipt pins the session
  causal context, observation time, and the configuration digest.

## Ports

All behavior lives behind the injected ports; the package persists and
retrieves nothing itself.

- `Kernel` — the bounded factory-authority port the deterministic Stage 05
  kernel (leaf #135) satisfies: `AdmitChangeIntent`, `ChangePlan`,
  `ChangeSetPreview`, `ReviewFindings`, `CancelChangeRun`. The port speaks
  the frozen contract vocabulary (`contractsv1` messages) because the
  kernel's canonical run, plan, candidate, and finding state commits under
  exactly those identities — the gateway never translates, stores, or
  reinterprets factory domain state. Port errors: `ErrUnknownRun` for any
  unknown/unauthorized/stale/revoked scope, `ErrIdempotencyConflict` for a
  conflicting key reuse, or the caller's context error unwrapped. Exact
  authenticated idempotent replays return the original outcome from the port
  without re-executing.
- `Clock` — receipt and observation time.

Authorization, approval revalidation, staleness checks, and idempotency
records are kernel obligations inside the port; the gateway authenticates,
validates, and binds receipts only. This package never imports
`services/brain`.

## Verification

```sh
go test -race ./gateway/internal/factoryapi           # from services/
bazel test //services/gateway/internal/factoryapi:factoryapi_test
```

The suite drives the handlers through a fake kernel at the port: the
missing/malformed/oversized boundary matrix, principal/tenant/session
mismatch, unmapped peer identity, idempotent replay and conflicting reuse on
admission and cancellation, stale-or-revoked denial equivalence
(byte-identical static shapes), pagination pass-through, contract-violating
port output failing closed (admitted state, plan shape, base binding, finding
disposition, cancel state), non-disclosing port failures, and cancellation
before and during port calls.
