// Package multimodal is the deterministic Stage 11 multimodal-source kernel.
// It admits one bounded text/Markdown, PDF, standalone PNG, or PCM WAV source
// under a versioned MultimodalSourceEnvelope, extracts modality-native anchors
// with deterministic sniffers, and owns the canonical source, evidence payload,
// readiness, revoke, and purge facts on the migration 007 insert-only tables.
// Original bytes and evidence JSON live in the encrypted ArtifactVault behind
// the PayloadStore port; SQLite only ever holds identities, digests, bounded
// structural facts, and lifecycle state.
//
// Admit may fail loud with typed pre-decode reasons (oversized, malformed,
// media_type_mismatch, encrypted_or_unsupported, partial_payload). Every other
// public denial shares one static typed error, ErrNotFoundOrDenied: unknown
// sources, caller mismatches, revoked or purged sources, conflicting
// idempotency reuse, and unauthorized operations are indistinguishable so the
// gateway can map them to the frozen non-disclosing response shape without
// branching. Exact authenticated idempotent replays return the original outcome
// without re-executing.
//
// JPEG, compressed audio, video, diarization, and speaker identity remain
// deferred (DEF-010). Synthetic anchors from honest extractors are acceptable
// for residual product proof; OCR/ASR quality is not the exit claim.
package multimodal
