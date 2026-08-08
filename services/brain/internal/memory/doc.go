// Package memory is the cohesive product-brain memory cortex: bi-temporal
// claims, episodes, utility-ranked recall, C1 consolidation probes, HippoRAG-
// style PPR multi-hop, RAPTOR summaries, and policy-gated agent memory tools.
//
// Design (SFS gardener + Graphiti + Lattice + HippoRAG, reimplemented in Go):
//
//   - Primary residual chunks stay immutable; memory projections live under
//     <brain>/memory/ and never block retrieval_ready.
//   - Claims carry valid_from/valid_to and supersession; conflicts are
//     contested or dual-cited — not silent overwrite (sfs-gardener conflict policy).
//   - Utility decay/reinforcement mutates ranking weights used at retrieve time.
//   - C1 hold-out probes can skip heavy gardener rewrites when prediction is healthy.
//
// Not a second binary: product-brain create/ingest/ask/gardener/memory commands
// own the surface. SCM session continuation packets remain out of scope.
package memory
