# Durable local storage adapters

This package attaches ArtifactVault metadata, evidence metadata, and key
reference ports to an already-open `localstate.Store`. `Open` requires schema
version 2 and never opens, migrates, locks, configures, or closes SQLite. The
authority Store remains the sole database owner and serializes every adapter
read and transaction through its bounded connection.

`Artifacts` persists opaque artifact locators, monotonic reservation fences,
complete frame manifests, and forward-only lifecycle state. Exact retries reuse
canonical metadata; changed, incomplete, stale, and cross-tenant requests fail
closed. A durable reservation is the crash boundary before the external object
effect; it does not make that effect and later command finalization atomic.
Artifact bytes and ciphertext remain in ArtifactVault's object store.

`Evidence` persists immutable tenant + brain + evidence records and typed
lineage. Tombstone immediately denies the record and deletes its rebuildable
edges. `KeyReferences` reads current, historical, legacy, and unreadable epoch
metadata. `InstallCurrentReference` creates one exact tenant-scoped current
reference; exact retries succeed, while conflicting or multiple-current state
fails closed. The operation accepts metadata only. On macOS, the runtime passes that concrete source to
`keyring.NewDarwinResolver`; this package never loads, creates, stores, logs, or
falls back for root-key material.

This package does not apply migrations, admit commands, authorize requests,
store payload bytes, rotate keys, or own SQLite sync or lifetime. Those remain
with the local authority composition, frozen schema, policy, Keychain, and
event-ledger boundaries.

Acceptance label:
`//services/brain/internal/localstorage:localstorage_test`.
