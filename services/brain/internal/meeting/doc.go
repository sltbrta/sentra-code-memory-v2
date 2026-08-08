// Package meeting is the deterministic Stage 07 meeting-transcript kernel. It
// admits one timestamped fixture transcript under explicit retention and a
// participant-notification reminder, answers with exact time-range anchors,
// and owns the canonical session, segment-payload, retention, revoke, and purge
// facts on the migration 006 insert-only tables. Transcript prose lives in the
// encrypted ArtifactVault behind the PayloadStore port; SQLite only ever holds
// identities, digests, bounded structural facts, retention codes, and lifecycle
// state.
//
// Every public denial shares one static typed error, ErrNotFoundOrDenied:
// unknown meetings, caller mismatches, revoked or purged meetings, conflicting
// idempotency reuse, and unauthorized operations are indistinguishable so the
// gateway can map them to the frozen non-disclosing response shape without
// branching. Exact authenticated idempotent replays return the original outcome
// without re-executing.
//
// Live capture, ScreenCaptureKit, calendar automation, and provider SDK capture
// remain deferred (DEF-002); this package implements the TUI-only import path.
package meeting
