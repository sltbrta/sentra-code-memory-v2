// Package outcomes admits sanitized Tracer 001 outcome facts after the draft
// PR step. Admitted facts are machine observations only: model proposals,
// prompts, secrets, and raw source never elevate into evidence. Raw traces
// remain under their original restricted scope (RawTraceSeparated must be
// true). Duplicate reingest of the same outcome bundle digest returns the
// original receipt.
//
// Status: [partial] Stage 06 L2 outcome admission — no gateway/TUI surface.
package outcomes
