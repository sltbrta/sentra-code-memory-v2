# Evidence ledger

The evidence ledger owns immutable evidence metadata: exact ArtifactVault
generation references, anchors, digests, tenant/brain scope, and typed lineage.
It never stores artifact bytes and it never treats an evidence ID as authority.

Every read uses the full tenant + brain + evidence key. Missing, inaccessible,
and tombstoned records return the same error. Lineage endpoints must already
exist within one tenant and brain; endpoint validation and edge insertion are
one repository transaction with tombstone. Tombstone immediately hides the
record and removes its rebuildable lineage edges. Evidence digests are canonical
lowercase 64-hex SHA-256 values, and opaque scope keys use length prefixes.

`MemoryRepository` is a deterministic test adapter, not production authority.
Stage 2 integration supplies the SQLite metadata repository.

Acceptance label: `//services/brain/internal/evidenceledger:evidenceledger_test`.
