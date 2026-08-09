# Local authority schema

The ordered SQL files are the canonical local SQLite schema contract.
Migration 001 is immutable authority history; migration 002 adds durable
ArtifactVault reservation, evidence, key-selection, and tombstone-receipt
metadata without rewriting version 1. Migration 003 adds only Stage 03 source,
approved-root reference, immutable source-revision identity, snapshot
membership,
generation/readiness, and tombstone metadata. Snapshot membership can reuse an
unchanged revision while enforcing one source object and path digest per
snapshot. Source and generation lifecycles are forward-only, and a current
generation requires non-pending readiness for all five supported languages.
Publication also requires snapshot membership to equal `path_count`; an empty
snapshot is valid only when both counts are zero. Published readiness and
snapshot membership remain immutable even when a newer generation
becomes current. Current generation sequences advance monotonically.
Migration 004 adds only Stage 04 conversation metadata: principal-scoped turns
with densely appended per-session sequence (each insert extends its session by
exactly one) and one query-idempotency record per (tenant, principal,
idempotency key) referencing the admitted user turn; a trigger rejects
idempotency rows that reference a non-user turn. Turn rows carry role
(`user`/`assistant`), terminal status (`active`/`failed`), and the encrypted
ArtifactVault payload identity plus its canonical digest; they never store
rendered prose. Assistant completions carry the query idempotency key under a
partial unique index, enforcing exactly one completion per admitted query and
resolving idempotent replay to the original outcome. Turn identities are
unique per principal so history paginates in a total (occurred-at,
turn-identity) order. Turns and idempotency records are insert-only: update
and delete are rejected by trigger.
Migration 005 adds only Stage 05 factory kernel metadata: principal-scoped
runs pinned to an exact Git base, one typed plan node set and required gate
roster per run, densely fenced per-leaf leases, exactly-once leaf results,
sequence-dense mailbox messages with exactly-once acknowledgements, typed
findings with exactly-once dispositions (dismissal requires recorded
evidence), per-operation idempotency records distinguishing exact replay from
conflicting reuse, one candidate preview per run, and rollback receipts.
Run, gate, and candidate lifecycle transitions append densely per aggregate
and stop at their terminal states. Leaf nodes carry scope, route, and grant
facts while orchestrator and review nodes carry none, enforced by a shape
check on the node row, and idempotency, mailbox, and rollback rows carry
foreign keys to their canonical runs and plan nodes. All factory tables are
insert-only: update and delete are rejected by trigger.
SHA-256 digests are 64 lowercase hexadecimal characters; Git object IDs are
40- or 64-character lowercase hexadecimal values. Approved-root identifiers use
the 64-character lowercase hexadecimal form.
Exact-search, symbol, and occurrence indexes remain rebuildable projections and
are not canonical SQLite tables. A runtime migration executor must enable
foreign keys before beginning a transaction, apply each version atomically,
and record it in `schema_migrations` only after every statement succeeds.

The schema stores metadata, receipts, policy epochs, audit links, artifact
manifests, evidence records, lineage, and opaque Keychain references. It must
never store artifact plaintext, ciphertext, or key material. Runtime WAL,
busy-timeout, replay, and writer serialization belong to the authority-kernel
and durable local-storage leaves.

`Migrations` embeds all checked-in files and returns a fresh, consecutive
descriptor slice. Production startup consumes this API and therefore never
depends on a repository-relative SQL path.

Run `bazel test //services/brain/internal/localstate/schema:schema_test`.
