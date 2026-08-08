// Package factoryapi implements the five frozen Stage 05 FactoryService RPCs
// — AdmitChangeIntent, GetChangePlan, PreviewChangeSet, GetReviewFindings, and
// CancelChangeRun — behind the authenticated owner-only gateway boundary.
//
// The package mirrors the Stage 03/04 gateway conventions exactly: the
// decoded message passes the generated buf.validate field, required-oneof,
// and CEL rules before anything else, the untrusted body caller cross-checks
// against the authenticated peer before any port invocation, the trusted
// principal is derived exclusively from the peer, and every constructed
// response — success and static denial alike — is revalidated against the
// frozen descriptors before return.
//
// All run authority lives behind the injected Kernel port, which the Stage 05
// deterministic factory kernel (leaf #135) satisfies. Unknown, unauthorized,
// stale, and revoked runs share the one static not_found_or_denied outcome
// with a rejected receipt and zero evidence refs; the boundary discloses no
// existence detail. This package never imports services/brain and holds no
// state between calls.
package factoryapi
