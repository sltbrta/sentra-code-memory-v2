# Private conversation history store

Status: **[shipped] composed behind the gateway query surface.** This package
persists the Stage 04 private principal-scoped session history on migration
004 and is composed by the gateway leaf (`queryapi`) through its exported
methods, behind the brain local-authority query facade. It registers no
handlers, opens no sockets, and keeps every rendered byte inside the
encrypted ArtifactVault.

## API

```go
store, err := conversation.Open(ctx, authorityDBPath, payloads, clock)
admission, err := store.Admit(ctx, conversation.Admission{...})      // user turn + idempotency, atomic
completion, err := store.Complete(ctx, conversation.Completion{...}) // exactly-once assistant turn
resolution, err := store.Resolve(ctx, tenant, principal, key)        // original outcome for a retry
page, err := store.History(ctx, tenant, principal, cursor, 50)       // principal's own turns
recovered, err := store.RecoverInterrupted(ctx)                      // restart sweep: failed, never replayed
```

- `Open` attaches to an already-migrated authority database (migration 004
  verified, fail-closed `ErrSchemaUnsupported`). The composing local authority
  owns migrations and the process owner lock; this store takes neither.
- `Admit` commits the user turn and its query-idempotency record in one
  serializable transaction, honoring the dense per-session sequence trigger.
  An exact retry returns the original `QueryID`/`UserTurnID` with `Replayed`;
  a reused key with a different request digest returns
  `ErrIdempotencyConflict` and mutates nothing. The request digest binds
  tenant, principal, source, generation, freshness, and query text — not the
  session, so a reconnect may replay an exact retry.
- `Complete` appends exactly one assistant turn per admitted query: `active`
  with the full grounded `query.Result` (answer, freshness, coverage, and
  projection, so replay reconstructs the original outcome byte-faithfully) or
  visibly `failed` with no answer. The owning session is resolved from the
  admission record, never from the caller. The schema's partial unique index
  enforces exactly-once completion per key; an exact completion retry
  replays, a differing one returns `ErrCompletionConflict`.
- `Resolve` returns the admitted identities and, once present, the
  completion: status plus the hydrated result for active completions. The
  gateway uses it to replay an idempotent Ask byte-faithfully.
- `History` paginates the authenticated principal's own turns in the frozen
  `(occurred_at_ms, turn_id)` total order through an opaque versioned cursor,
  at most one hundred per page. Cross-principal and cross-tenant turns are
  never selected; history is structurally inexpressible across principals.
- `RecoverInterrupted` appends one visibly failed assistant turn for every
  admitted-but-uncompleted query. It runs once at restart before the query
  surface serves and is idempotent.

## Vault payloads

Rendered turn bytes live exclusively in the encrypted ArtifactVault behind
the narrow `PayloadStore` port; SQLite stores only the opaque artifact
identity and canonical SHA-256 digest the migration allows. `NewVaultPayloads`
adapts the Stage 02 vault (`artifactvault.Vault` + `keyring.Resolver`):
every payload is a fresh single-generation artifact staged and published
through the canonical lifecycle under the tenant's current key epoch. The
`frameBytes` argument must match the vault's configured frame size.

Hydration reverifies the payload digest against the canonical metadata before
any use; a tampered, missing, or tombstoned payload fails the whole read with
`ErrPayloadUnavailable` rather than silently skipping a turn. Role shape is
revalidated on hydration: user turns carry text, active assistant turns carry
an answer, failed assistant turns carry neither.

## Failure model

- Turn and idempotency rows are insert-only; update and delete are rejected
  by trigger, and the store issues no such statements.
- Turn status is terminal at commit. An interrupted assistant turn is visibly
  `failed` and is never replayed as fact; the client must use a new
  idempotency key to retry a failed query.
- Vault staging precedes the metadata transaction, so a crash can leave an
  unreferenced immutable artifact but never a metadata row without bytes.
- The store serializes operations behind one mutex; cross-process single-writer
  exclusion stays with the authority owner lock.

## Verification

```sh
go test -race ./brain/internal/conversation        # from services/
bazel test //services/brain/internal/conversation:conversation_test
```

The suite runs against a real migrated SQLite database (Stage 02 sessions
plus migration 004) and covers atomic admission, replay/conflict idempotency,
exactly-once completion, visible failed turns, crash-mid-completion recovery
across reopen, cursor bounds and total order, cross-principal invisibility,
digest-verified hydration against tampered payloads, live trigger enforcement
on store-written rows, and vault round trips through the real encrypted
adapter.
