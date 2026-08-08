# ArtifactVault

ArtifactVault is the only Stage 2 authority for persisted artifact bytes. It
encrypts an exact-length stream into independently authenticated frames, stores
opaque immutable objects, verifies the complete generation, and advances a
metadata generation pointer conditionally. SQLite-facing `Repository` values
contain manifests, lifecycle, opaque locators, offsets, and digests only—never
plaintext, ciphertext, DEKs, or root keys.

The local adapter implements the S3 semantics needed by the v1 boundary without
running an S3 server: create-if-absent immutable objects, exact reads, and
idempotent deletes. Descriptor-relative roots and shard inode checks reject
symlink replacement. Files are created through owner-only `.part` files,
synced, linked without overwrite, and directory-synced on publication and
purge. Cleanup and durability errors are returned. Callers receive no path or
generic bucket API.

`StageContent` and `HydrateRange` are the concrete byte-bearing seams because
the frozen shared `ArtifactVault` contract carries metadata only. `Stage` and
`ReadRange` satisfy that contract by verifying the concrete operation. The
frozen stage request has no authority fence, so upstream admission must validate
the current capability fence before calling this package; this package does not
claim to enforce that absent field.

Reads are bounded before lookup, resolve the exact key epoch, authenticate AAD
containing tenant, artifact kind, opaque locator, generation, and frame index,
and quarantine integrity or key-history failures. Tombstone denies reads before
purge deletes primary objects. Purge makes no backup-erasure claim.

Generation publication accepts only an exact retry after success. Purge derives
the exact tombstoned generation from its canonical length-prefixed receipt, so
multiple generations may safely reuse one key epoch. Opaque composite keys use
length prefixes rather than delimiter concatenation.

`MemoryRepository` is for tests and isolated fixtures only. Production
composition supplies the SQLite repository frozen by Stage 2 A0.

Acceptance label: `//services/brain/internal/artifactvault:artifactvault_test`.
