# Grounded-query gateway handlers

Status: **[shipped] composed behind the authenticated owner-only gateway.**
This package implements the four frozen Stage 04 `QueryService` RPCs — `Ask`,
`ListSources`, `GetHistory`, and `GetStatus` — behind the authenticated
owner-only gateway boundary. The runtime command (S04-I0) composes
`NewHandler` with the real engine, conversation store, source catalog, and
authorizer, and mounts the methods on the frozen
`/ouroboros.contracts.v1.QueryService/<Method>` procedures of the same Unix
socket as the Stage 02/03 procedures.

## API

```go
handler, err := queryapi.NewHandler(queryapi.Config{
    Engine: engine, Conversations: store, Sources: catalog,
    Authorizer: authorizer, Clock: clock, ConfigurationDigest: digest,
})
ask, err := handler.Ask(ctx, peer, request)              // AskRequest  -> AskResponse
sources, err := handler.ListSources(ctx, peer, request)  // ListSourcesRequest -> ListSourcesResponse
history, err := handler.GetHistory(ctx, peer, request)   // GetHistoryRequest -> GetHistoryResponse
status, err := handler.GetStatus(ctx, peer, request)     // GetStatusRequest -> GetStatusResponse
```

Each method takes the authenticated `localauthority.PeerContext` separately
from the complete decoded request, mirroring the Stage 03 `IngestionAuthority`
port shape, so the composing server can mount the methods on the frozen
`/ouroboros.contracts.v1.QueryService/<Method>` procedures.

## Boundary rules

- **Validate, then identity, before any port.** Every method first executes
  the generated `buf.validate` field, required-oneof, and CEL rules on the
  decoded message (the Stage 03 convention), then cross-checks the untrusted
  body caller — principal, tenant, and both session references — against the
  authenticated peer. Both gates run strictly before any port invocation:
  validation failures return `ErrInvalidRequest`, and a mismatch or unmapped
  peer returns `ErrRequestDenied`, which the transport maps to its static
  `request-denied` shape. The trusted principal is always derived from the
  peer, never the body.
- **Every response revalidated.** Every constructed response — success and
  denial alike — is revalidated against the frozen descriptors before return;
  a contract violation returns `ErrInvalidResponse` (a defect, mapped to
  `response-invalid`).
- **Static non-disclosure.** Unknown, unauthorized, or revoked reads return
  the one static `not_found_or_denied` outcome with a rejected receipt and
  zero evidence, in-band as the frozen contract freezes. Port errors with no
  contract shape return `errPortFailure` and never carry upstream text.
- **Current authorization.** `Ask` evaluates `ActionQuery` on the source
  before admission; a denial commits nothing. `GetHistory` reauthorizes
  hydration with `ActionHydrate` on the `conversation` scope before any
  payload read. The engine re-authorizes inside its own funnel on top of
  these checks.

## Ask funnel

`Ask` threads identity → authorization → admission → engine answer →
completion:

1. Authorization denial returns the static denial before any turn is written.
2. Admission commits the user turn and idempotency record atomically. A
   conflicting key reuse returns the static denial without mutation; an exact
   retry resolves the admitted key to its original outcome — an active
   completion rebuilds the original success byte-faithfully, a failed
   completion stays terminal, and an admitted-but-uncompleted (crashed) query
   is marked failed exactly once before the static denial.
3. Engine errors (`ErrUnknownScope` for unknown/revoked/unservable scopes)
   commit a visibly failed assistant turn, then return the static denial.
4. Transport-context cancellation commits no assistant turn: the admitted
   user turn persists, and restart recovery or the replay path marks it
   failed later. Cancellation is returned as the context error, not a
   contract response.
5. The public response is built and validated BEFORE the completion commits,
   so the stored terminal state always matches a returnable disposition:
   unavailable freshness facts or contract-violating engine output become a
   visibly failed completion (plus the static denial or `ErrInvalidResponse`),
   never an active one no caller can receive. A validated response commits
   exactly one active completion: receipt bound to the admitted query
   identity, claims stamped `AUTHORITY_CLASS_MODEL_PROPOSAL`, freshness
   assembled from the engine's pinned disclosure plus the catalog's immutable
   generation facts (snapshot identity, policy digest, five P5 lanes).

## Ports

All behavior lives behind the injected ports; the package persists and
retrieves nothing itself.

- `Engine` — `Answer`/`Status`, satisfied by the L1 engine through a thin
  field-for-field adapter (`query.Query`/`query.Result`/`query.SourceStatus`
  mirror the port types exactly). Port errors: `ErrUnknownScope`, or the
  caller's context error unwrapped.
- `Conversations` — `Admit`/`Complete`/`Resolve`/`History`, satisfied by the
  migration 004 conversation store through an adapter mapping
  `conversation.ErrIdempotencyConflict`/`ErrUnknownAdmission`/
  `ErrCompletionConflict`/`ErrUnknownSession` onto the package's exported
  sentinels (`ErrUnknownSession` maps to `ErrRequestDenied`).
- `SourceCatalog` — authorized source pages, immutable per-generation facts,
  and source references; unknown/unauthorized/revoked scopes return
  `ErrUnknownScope`. Revoked sources never list.
- `Authorizer` — current-relationship evaluation mirroring `query.Authorizer`
  (`Principal`/`Action`/`Decision` shapes are identical).
- `Clock` — receipt and observation time.

The deterministic synthesis adapter and any provider egress are composed
inside the engine by the runtime command, not here.

## Verification

```sh
go test -race ./gateway/internal/queryapi           # from services/
bazel test //services/gateway/internal/queryapi:queryapi_test
```

The suite drives the handlers through fakes at every port: the
missing/malformed/oversized boundary matrix, principal/tenant/session
mismatch, authorization denial and backend failure, duplicate and conflicting
idempotency, replay of active/failed/crashed completions, engine scope
failure committing a failed turn, provider failure and projection-rebuild
abstention mapping, cancellation before and during the engine and before
completion, contract-violating engine output, and response freshness.
