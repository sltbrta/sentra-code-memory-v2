// Package orgscope is the issue #311 fail-closed vertical slice and issue #312
// hermetic recovery drill contract for tenant/scope isolation, SCIM-shaped
// lifecycle with revocation receipts, and erasure tombstones across in-process
// content projections (primary store, search/index/claims/graph, query cache,
// session history/replay, backup/restore/rebuild). Recovery receipts pin
// generation/config digests, contiguous queue replay, current ACL state, RPO/RTO
// calculations, projection rebuild, complete tombstone scans, non-resurrection
// probes, substrate matrices, and injected failure points.
//
// The scheduled/runnable wrapper adds a context deadline and immutable,
// durable retention for both positive and negative receipts.
//
// Substrate honesty: this package proves the product contracts over hermetic
// in-process substrates in the same style as companymode's FakePostgres and
// FakeS3. Each substrate receipt carries its hermetic/provider-adapter
// boundary, and ProductionCertified is invariantly false. The built-in
// matrix does NOT exercise live filesystems, PostgreSQL RLS, vector services,
// HotLex volumes, OpenFGA cloud tuples, network SCIM, KMS crypto-shredding,
// production graph/claims/cache/object providers, queues, regional writer
// fencing, or live-provider RPO/RTO; those remain open under issues #311/#312
// (parent #306, gate #307) and DEF-015.
//
// Non-disclosure: every denial is the single error ErrDenied
// ("orgscope: not_found_or_denied"), so callers cannot distinguish a missing
// resource from a forbidden one. Audit entries record item ids and counts,
// never memory content, and deliberately survive erasure.
package orgscope
