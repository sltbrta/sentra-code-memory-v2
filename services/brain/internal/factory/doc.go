// Package factory is the deterministic Stage 05 factory kernel. It admits one
// approved ChangeIntent pinned to the exact current Git base, compiles the
// frozen typed one-layer DAG (one orchestrator, one to three prefix-disjoint
// leaves, at most one fresh review node, and the four non-removable gates),
// and owns the canonical run, plan, lease, gate, candidate, and finding facts
// on the migration 005 insert-only tables. Prose and large payloads — goal
// text, admitted intent bytes, candidate preview bytes, message payloads, and
// finding summaries — live in the encrypted ArtifactVault behind the
// PayloadStore port; SQLite only ever holds identities, digests, bounded
// structural facts, and lifecycle state.
//
// Every public denial shares one static typed error, ErrNotFoundOrDenied:
// unknown runs, caller mismatches, policy denials, stale bases, stale fences,
// conflicting idempotency reuse, and operations against terminal runs are
// indistinguishable, so the gateway can map them to the frozen non-disclosing
// response shape without branching. Exact authenticated idempotent replays
// return the original outcome without re-executing.
//
// The kernel is safe for concurrent use: one mutex serializes every operation
// and the underlying authority database is single-writer, matching the
// durability posture of the Stage 04 conversation store.
package factory
