# Bounded local ingestion authority

This package is a **backend-ready internal leaf** for one owner-approved local
Git repository. It does not make Stage 03 user-reachable: gateway, contract
mapping, search/index lanes, and TUI integration remain separate work.

## Authority and public surface

`New` binds an `Authority` to one absolute bootstrap root, absolute Git
executable, tenant, brain, repository, configuration digest, no-follow policy,
deadline, and explicit resource bounds. Authorization happens before this
package: callers may construct it only from the owner-protected bootstrap
configuration, never request-body paths.

`Admit` publishes the first complete source-snapshot generation. `Reconcile`
compares one complete committed tree with another, derives deterministic
revisions and add/modify/exact-rename/delete records, and advances the
source-snapshot pointer under the expected generation and commit. `ObserveHints`
records bounded watcher coverage only; hints never read bytes, publish, or prove
deletion. Overflow is a saturating coverage-loss flag and does not consume
path-hint capacity.
`Revoke` applies immediate deny, and `Tombstone` removes transient path-bearing
state after revoke. `Rebuild` proves a clean Git-tree scan equals the current
manifest. `HydrateCurrent` returns caller-owned records pairing each
`FileRevision` with exact committed bytes under an expected-generation fence
and caller-supplied file/byte bounds. `MarshalBinary` persists path-free
lifecycle metadata; `Restore` rebuilds active manifests from the exact commit
before returning.

Lifecycle checks and publication commits are serialized by one mutex. `Admit`,
`Reconcile`, `Rebuild`, and `HydrateCurrent` perform bounded Git and delta work
outside the mutex, then verify their lifecycle and source-snapshot fences. A
concurrent revoke therefore commits immediate deny and prevents an in-flight
scan from returning or publishing an active result.
The package starts no watcher, goroutine, server, or database transaction.
Callers receive defensive copies of manifests. Exact successful operation
retries return their original source-snapshot generation while active and fail
closed after revoke. `Generation.ExpectedPreviousID` is the atomic publication
CAS input, and incomplete source snapshots are never representable through this
API.

Checkpoint JSON includes a deterministic integrity digest and rejects altered,
unknown, truncated, trailing, configuration-mismatched, or internally
inconsistent metadata. The digest detects corruption but is not an
authentication mechanism; the checkpoint still belongs in the encrypted
canonical metadata authority. Retained operation receipts are opaque exact-retry
state rather than content authority. A persistence adapter may separate exact
retry history while retaining the bounded current/parent lineage receipts the
checkpoint validates. Receipts do not grant repository paths or content access.

## Git, identity, and policy

All content comes from `git ls-tree` plus `git cat-file --batch` for an exact
commit/tree. Commands use explicit argv, an absolute executable, isolated
configuration/environment, output limits, and context deadlines. Working-tree
bytes, dirty state, checkout filters, remote operations, shell interpolation,
submodules, and filesystem traversal are not used. Git symlink blobs are hashed
and recorded as links; targets are never followed.

The root may opt into committed root-level `.gitignore` and
`.ouroborosignore`. Both accept only blank/comment lines, exact
repository-relative paths, trailing-slash directory prefixes, and per-segment
`*`/`?`. Negation, `**`, absolute paths, dot segments, backslashes, bracket
patterns, whitespace-altered rules, and nested ignore files fail closed. The
two rule sets are exclusion-only and cumulative.

IDs and digests are lowercase SHA-256 over length-delimited UTF-8 fields. The
approved-root and snapshot-manifest identities use the domains and field order
frozen by `SPEC-DELTA-001`. File revision IDs bind source, path digest, Git blob
OID, and mode. Rename pairing requires exact blob OID and mode equality and is
stable under duplicate content by sorted old/new paths.

## Exact limits and failure behavior

The composition root must provide positive limits for file count, path bytes,
per-file bytes, total included bytes, retained idempotency records, and Git
command duration. Minimal v1 configuration caps are 100,000 files, 4,096 path
bytes, 16 MiB per file, 64 MiB total included bytes, 1,000,000 normal
idempotency records, and ten minutes per Git command. File-count/path-limit
combinations whose worst-case tree listing exceeds the 128 MiB Git output
buffer are rejected. Idempotency keys are at most 512 bytes and are retained
only as SHA-256 digests. Revoke and tombstone each have one reserved receipt so
normal receipt exhaustion cannot block lifecycle denial or cleanup.
Hydration requests must provide positive file and total-byte bounds no larger
than the bootstrap configuration; exceeding either request bound returns
`ErrLimit` before content is returned.

Errors are static sentinels suitable for `errors.Is`: invalid input,
unsupported policy, out-of-root path, stale generation, revoked, tombstoned,
Git failure, configured-limit exhaustion, and idempotency conflict. Git stderr,
repository paths, and blob bytes are never included in returned errors.
Cancellation and deadlines propagate from the supplied context.

## Non-goals

This package publishes atomic source-snapshot generations only. Product/current
generation promotion across all five P5 readiness dimensions belongs to the
Stage 3 integration tracked by issue #93. This package does not expose a network
API, authenticate principals, manage the approved-root registry, persist SQLite
rows or ArtifactVault bytes, traverse submodules, implement a filesystem watcher,
index/search code, run compiler/LSP semantics, use embeddings/media/models,
contact remotes, or perform distributed execution. It does not claim
product-complete generation, arbitrary-root support, dirty-file ingestion,
ignore reinclusion/precedence, symlink following, or watcher-authoritative
deletion.

Acceptance label:
`//services/brain/internal/ingestion:ingestion_test`.
