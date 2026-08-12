// Package sessionlog records a repo-local, append-only, bounded, privacy-safe
// event stream for coding-agent sessions (Phase 4: issues #26–#31).
//
// The log is opt-in: the fast lexical codeserve/codecrawl path never touches
// it, so existing JSONL behavior is unchanged. Callers that want session
// continuity append one event per observable step (task start, context served,
// read, refresh, edit, test, failure, compaction, completion) and later replay
// the stream into a deterministic session summary, a continuation packet, or a
// compaction packet.
//
// Privacy model: events prefer pointers (repository/tree/path/range/symbol/
// handle) over copied source. Free text is explicitly capped and never holds
// file contents. Stale memory is disclosed and superseded content is excluded
// by default (#28); durable events require provenance before admission (#29);
// recall abstains when memory is weak or unrelated (#30); replay rebuilds the
// same projection from the persisted stream as live state (#31).
package sessionlog

// Schema is the on-disk JSONL event-contract revision persisted with every
// event so future readers can detect and reject incompatible streams.
const Schema = "sentra-scm.sessionlog/v1"
