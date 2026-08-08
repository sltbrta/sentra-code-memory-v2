# Local authority runtime

**RETIRED as product path (ADR 0022).** Product create/ingest/ask/continual/
gardener live on `internal/hosted` + `product-brain`. This package is frozen
regression/substrate only — do not add product features here.

---

This package composes the Stage 2 SQLite command/audit ledger with the real
encrypted ArtifactVault and tenant-and-brain-scoped evidence ports. It exposes
session open, metadata admission, bounded encrypted reads, tombstone/purge, and
status to an outer gateway adapter. Every artifact operation receives a current
authorization function and collapses policy, existence, integrity, and audit
failures to a static denial.

## Bounded committed-Git ingestion

`DurableConfig.Ingestion` optionally enables one approved absolute Git root.
The configuration fixes `.gitignore` plus `.ouroborosignore` handling and
records symlinks without following them. It also requires an absolute Git
executable, opaque repository ID, command timeout, and explicit file, path,
byte, and idempotency limits. A nil configuration preserves the Stage 2
artifact-only runtime and performs no repository access.

The proto-agnostic public surface is `AddSource`, `GetSourceStatus`,
`SearchCode`, `ReconcileSource`, and `RevokeSource`. Every request carries the
peer-authenticated identity, exact configuration digest, fixed policy selector,
fence, and current authorization callback. Authorization is evaluated against
the configured brain before source lookup, Git hydration, or index work.
Denial, absence, revocation, stale generations, and storage disagreement expose
only `ErrDenied`.

Admission and reconciliation use the internal committed-Git authority and read
only exact Git objects. All canonical file and no-follow symlink revisions are
published to SQLite, while `.go`, `.ts`/`.tsx`, `.py`, `.rs`, and `.java` files
also enter the deterministic code index. Exactly five readiness lanes publish
atomically with the source pointer. Malformed P5 files remain searchable as
lexical occurrences and mark their language lane degraded.

Search is exact-only and generation-pinned. It supports all occurrences,
definitions, or references with a limit of 100. Stable opaque cursors bind the
generation, query, kind, and offset; results carry an immutable Git blob,
content digest, path, exact range, and matched spelling. The runtime has no
embedding, model, native watcher, or working-tree-content claim.

Reconciliation builds a candidate without replacing the current searchable
snapshot, then performs one digest-bound `PublishGeneration` transaction before
swapping memory state. It uses incremental `codeindex.Apply` when an in-memory
base exists. Source revocation applies the internal deny overlay before the
atomic SQLite tombstone/pointer removal; the revoked flag denies every later
read and query checkpoint while immutable generations stay resolvable to the
query corpus so an already admitted query keeps its pinned freshness and
coverage truthful.

After an authenticated session is reopened, checkpoints lazily rebuild and
verify the exact commit, tree, snapshot, manifest, and P5 index. Stage 3 v1
supports the admitted generation and one reconciled 100-record delta: sequence
two restarts first rebuild generation one, then reconcile the persisted current
commit, and both generations stay projected for pinned query. A revoked
checkpoint still rebuilds its immutable generations while every authorization
checkpoint denies; a third reconciliation is denied before publication.

## Bounded grounded-query surface

`OpenQuerySurface` composes the Stage 04 query surface over one durable
ingestion-configured runtime: the ACL-first grounded-query engine over the
retained current and superseded generations (canonical revisions, rebuildable
projection, hydrated bytes), the migration 004 conversation store on the same
authority database with turn payloads in the same encrypted vault, and the
migration 003 catalog reads. `RecoverInterrupted` runs once at startup before
the surface serves. Every public type is a thin alias over the internal
engine/store contracts, so the gateway command composes without importing
brain-internal packages and the facade remains the only composition point.

`OpenDurable` is the production composition boundary for one owner-locked
SQLite database, the embedded ordered migrations, durable storage metadata,
one declared current key reference, and one encrypted object root. The supplied
resolver must already own the exact current secret. `OpenDarwin` builds that
resolver from SQLite metadata and an existing macOS Keychain item through the
fixed `/usr/bin/security` runner; startup never creates key material.
Startup resolves the declared material before installing metadata, then stores
an HMAC-SHA-256 commitment bound to the tenant, provider reference, and epoch.
The root key and provider selector remain outside SQLite. Startup and every
live current/configured-epoch resolution recompute the commitment, so changed
material under the same selector fails closed before cryptographic use.

`PrepareArtifact` is the sole trusted pre-publication seam for ingestion and
fixture composition. It serializes with runtime operations and stages encrypted
frames only. It is not exposed as a client command and does not authorize,
publish, admit evidence, or append command history. Stage 3 owns its ingestion
caller and broader source lifecycle. Only `OpenDurable` installs the explicit
tenant and current-epoch staging scope; the legacy `New` constructor cannot
stage bytes.

`NewStorage` remains available for focused composition tests with explicit
ports. In-memory implementations are test-only and do not establish a
durability claim. The durable path has no file-key fallback.

The runtime reauthorizes and verifies audit state before `Reserve` durably
claims an exact command. It reauthorizes the reserved canonical operation again
immediately before every storage effect, including replayed reads. A revocation
between those checks leaves an accepted, resumable reservation without touching
storage. It then runs the idempotent ArtifactVault/evidence effect and calls
`Finalize` with the canonical command returned by the reservation. Accepted
state survives a crash and resumes; completed admit and delete retries do not
rerun effects. An exclusive SQLite owner plus the runtime mutex preserves the
Stage 2 single-process writer model.

Every public runtime operation shares the lifecycle mutex. `Close` waits for an
active operation, closes dependencies once in reverse order, and makes later
session, status, command, and staging calls fail before dereferencing SQLite.

Reads and deletes resolve immutable persisted generation metadata before their
effect and use its canonical key epoch. Only new admission uses the configured
current key epoch; advancing the daemon epoch therefore cannot strand reads or
purges of older encrypted generations.

Session admission is itself a deterministic internal `session.open` command,
event, receipt, and audit unit derived only from authenticated identity. Exact
retries return that canonical receipt. A valid authenticated artifact command
denied by current authorization similarly records one non-disclosing
`authorization` event and rejected receipt without touching artifact storage;
the command row's `completed` status means processing finished, while the
receipt's `rejected` status is the outcome. Status verifies the exact persisted
session and audit chain and returns the real tenant event watermark.
Internal session-admission and authorization-denial command/event identifiers
use readable purpose prefixes plus SHA-256 digests of trusted facts, so maximum
valid external identifiers never expand canonical index keys. Once a rejected
receipt is canonical, later policy allowance returns that rejection as a
terminal replay before metadata lookup, hydration, or any other effect.

The object store and SQLite are not one physical transaction. Durable accepted
reservations and idempotent immutable effects provide bounded crash recovery;
distributed recovery remains deferred to later ingestion and execution stages.
