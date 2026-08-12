// Package contextpack provides deterministic, bounded context packing for
// agent-facing retrieval payloads (Phase 1: issues #7-#11).
//
// # Scope
//
//   - Hard byte/token budgets with relevance-proportional allocation and a
//     direct-source floor (#7).
//   - Explicit truncation/omission metadata and stable progressive-disclosure
//     expansion handles; stale handles fail clearly (#8).
//   - Session-aware source deduplication keyed by content fingerprint and
//     range, returning back-pointers for repeated unchanged source (#9).
//   - Render modes: full, signatures, skeleton, compact (#10).
//   - Bounded resource Limits/Governor (workers, candidates, output bytes,
//     wall time) with fail-safe enforcement and visible reports (#11).
//
// # Determinism
//
// Packing is a pure function of its inputs: no clocks (except the injected
// Governor time source), no map iteration in output, stable ordering by
// (score desc, path, start line). Identical inputs produce byte-identical
// results.
//
// # Non-goals
//
// No external dependencies, no daemon, no persistence. Session state lives in
// process memory (Registry); callers own its lifetime.
package contextpack
