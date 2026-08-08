// Package query implements the bounded Stage 04 grounded-query engine over the
// Stage 03 committed-Git corpus.
//
// The engine is a pure library: it registers no Connect handlers, opens no
// sockets, and persists nothing. The gateway leaf composes it behind the
// authenticated local authority and maps its domain results onto the frozen
// query.proto contracts. Retrieval is ACL-first: a principal's query only
// reads the occurrence projection and canonical revision facts its
// relationships allow, and denied, revoked, or unknown support collapses to a
// byte-identical absent_support abstention. Every answer pins exactly one
// immutable generation, hydrates candidate evidence from canonical bytes with
// digest and anchor verification, packs evidence under frozen bounds, and
// synthesizes claims behind a model-adapter boundary that fails closed.
package query
