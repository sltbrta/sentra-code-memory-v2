# Local authority state

`Store` is the single writer for one SQLite WAL authority database. `Open`
requires an explicit absolute database path and retains an exclusive process
owner lock until `Close`. Its composition root supplies consecutive immutable
migrations; the store applies each unapplied version in its own transaction.
Version 1 remains unchanged and version 2 adds durable storage-adapter metadata.
SQLite runs with WAL and `synchronous=FULL`.

`ReadMetadata` and `WriteMetadata` are the narrow executor used by attached
storage adapters. They share the Store's mutex and sole connection; no adapter
may open a second handle or own database lifetime.

Effectful commands follow one durable lifecycle:

1. `Reserve` commits the canonical command and idempotency mapping as accepted.
2. The caller performs the external effect idempotently using that canonical
   reservation identity.
3. `Finalize` atomically appends events, audit links, outbox entries, the
   receipt, projection watermarks, and the completed status.

An accepted reservation survives restart and is resumable. Exact completed
retries return the original canonical command and receipt. Global command-ID
collision checks are an internal schema safeguard; external boundaries map
reservation failures to static denial and never reveal command existence.
The durable reservation is a crash-recovery boundary; an external artifact
effect and a later `Finalize` call are intentionally separate operations.

The store contains metadata only. Artifact bytes and encryption keys belong to
the ArtifactVault and key-root packages. SQLite files and WALs are never sync
artifacts.

## Stage 03 ingestion persistence

The path-free ingestion surface is deliberately limited to three operations:

- `PublishGeneration` atomically persists the authenticated command, immutable
  source/root/snapshot/revision membership, exactly five P5 readiness rows, a
  complete generation, its monotonic current-pointer CAS, and the receipt.
  Advancing the pointer also tombstones revisions removed or replaced relative
  to the prior complete snapshot; unchanged revision identities stay active.
- `LoadIngestionCheckpoint` authenticates an exact persisted session and loads
  only opaque source/root identifiers, configuration and policy digests, Git
  commit/tree OIDs, lifecycle epochs, and the current or last revoked
  generation. It never returns an absolute or repository-relative path.
- `RevokeIngestionSource` atomically denies the source, tombstones all active
  revisions plus the source, removes the current pointer, and records the
  canonical receipt. An exact command retry returns the original receipt.

The composing brain runtime must derive and validate source-object IDs,
predecessor lineage, entry kind/media type, optional P5 indexing language, and
the five independent readiness lanes. It computes `GenerationPublicationDigest`
or `IngestionRevocationDigest` only after the full operation target is assembled
and places that result in the command envelope; the Store recomputes it before
any lookup or write, preventing an idempotent retry from changing scope or
target. The runtime supplies the approved absolute root only
to the Git ingestion authority; this Store receives the opaque approved-root
ID. A successful publication is the acknowledgement boundary: no caller may
acknowledge a generation before `PublishGeneration` returns.

## Stage 04 source catalog

Two read-only queries serve the grounded-query source catalog over the same
tables, scoped to one exact tenant/brain/source triple and carrying no paths:

- `LoadIngestionSourceState` returns the source's repository identity,
  lifecycle state, and current complete generation pointer.
- `LoadIngestionGenerationFacts` resolves one published complete generation's
  immutable facts — snapshot identity, policy digest, watermark, and the five
  P5 readiness lanes. Facts never change per generation identity, so a
  superseded or revoked-source generation resolves exactly as published; the
  composing gateway owns authorization and revocation-aware denial, and an
  incomplete `building` row never resolves.
