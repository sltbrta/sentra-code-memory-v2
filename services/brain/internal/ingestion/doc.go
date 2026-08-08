// Package ingestion publishes atomic source-snapshot generations from one
// preapproved local Git repository. Git objects are the authority: working-tree
// bytes and watcher events can never publish or delete a source revision.
//
// The package keeps repository-relative paths only in transient manifests.
// MarshalBinary deliberately persists only opaque IDs, digests, Git object IDs,
// and lifecycle metadata; Restore rebuilds and verifies manifests and deltas
// from Git. HydrateCurrent returns bounded caller-owned committed content without
// consulting the working tree. Product readiness and promotion are integration
// concerns.
package ingestion
