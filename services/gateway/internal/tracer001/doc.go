// Package tracer001 implements the Stage 06 Tracer 001 public path as a
// composition facade behind the authenticated owner-only gateway boundary.
//
// Stages 03–05 services remain the product RPC surface; this package does not
// invent merge/deploy messages or product RPCs in contracts. It advances one
// causal loop step at a time and returns frozen tracer records
// (TracerRun / TracerStepReceipt / DraftPrReceipt / OutcomeFact) after
// protovalidate and peer cross-check. Unknown, unauthorized, stale, and
// revoked scopes share the static not_found_or_denied shape: inaccessible ≡
// absent. All domain work lives behind the injected Path port (fakes in
// package tests; real composition is I0 scope).
package tracer001
