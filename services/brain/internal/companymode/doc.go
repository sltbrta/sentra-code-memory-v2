// Package companymode implements the bounded Stage 10 company profile.
//
// V1 proves two principals in one company tenant over an in-process
// PostgreSQL-shaped event ledger and an S3-compatible object store, with
// tenant isolation, event/blob sync (never SQLite transfer), backup/restore,
// disposable recovery/erasure drills, bounded admission, and a 20-user
// mixed-load smoke. Authorization uses
// the OpenFGA-compatible in-process evaluator expanded with company fixtures
// (issue #72 partial; HTTP client + hermetic dual-run landed; live store
// conformance remains DEF-015).
//
// Non-goals: Kubernetes, 1,000-user certification, AWS/EKS, multi-region,
// air-gap packaging, and production SLO claims.
package companymode
