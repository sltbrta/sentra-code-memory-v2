// Package conversation owns the durable principal-scoped private session
// history the Stage 04 query slice freezes on migration 004.
//
// The store persists only the metadata the migration allows — opaque turn,
// tenant, principal, session, and idempotency identities, canonical SHA-256
// payload digests, encrypted ArtifactVault payload identities, occurrence
// times, roles, and terminal statuses — inside the same single-writer SQLite
// authority database as the Stage 02 session ledger it foreign-keys against.
// Rendered turn bytes (query text and assistant results) live exclusively in
// the encrypted vault behind the narrow PayloadStore port; the schema stores
// no prose, no query text, no citation sets, and no search projection.
//
// Admission commits the user turn and its query-idempotency record in one
// transaction so an exact idempotent retry replays the original disposition
// and a conflicting key reuse mutates nothing. Completion appends exactly one
// assistant turn per admitted query — active with the full grounded result or
// visibly failed — and the schema's partial unique index enforces exactly-once
// completion per key. Restart recovery marks admitted-but-uncompleted queries
// failed rather than replaying them. History paginates the principal's own
// turns in the frozen (occurred_at_ms, turn_id) total order through an opaque
// cursor; cross-principal history is structurally inexpressible.
package conversation
