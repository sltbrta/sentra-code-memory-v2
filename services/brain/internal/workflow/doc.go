// Package workflow produces machine-readable, bounded agent-facing artifacts
// for the Phase 5 issues #32–#34: action envelopes (#32), evidence reports
// (#33), and fail-closed candidate ChangeSet validation (#34).
//
// All output is deterministic (stable field order, lexical-count maps) and
// content-safe (digests and pointers, never source bytes). The package depends
// only on Go stdlib and codecrawl content hashing, so it adds no dependencies
// and does not touch the existing codeserve JSONL path.
package workflow

// Schema is the on-disk/report contract revision.
const Schema = "sentra-scm.workflow/v1"
