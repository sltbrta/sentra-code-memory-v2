// Package hosted is the ONE product company-doc runtime (Client).
//
// Store adapters + substrates (ADR 0024 — one pipeline, not two products):
//
//	chunks:  path2 | product_neon | memory | local_fs
//	queue:   sqlite (durable) | postgres (parallel SKIP LOCKED) | memory→sqlite | ephemeral | none
//	cortex:  fs (memory.Open) | none
//	dense:   none | qdrant | sqlite | postgres | faiss HTTP | memory
//	llm/embed/ranker: hosted | mlx (OpenAI-compatible) | none
//	workers: OUROBOROS_BRAIN_WORKERS / GOMAXPROCS — local burst fleet (not hosted Modal)
//
//	path2        — SMF Neon FTS + Qdrant ANN (ERB full-bench)
//	product_neon — product_chunk_metadata + structure tables
//	memory       — in-process chunks (queue still durable when Dir set)
//	local_fs     — durable FS projection (OpenLocal / CreateLocal)
//
// Hosted residual prefers remote substrates when configured (Neon + Qdrant + vendor APIs).
// Completely local residual: chunks=fs, queue=sqlite, cortex=fs, dense=sqlite, llm/embed/ranker=mlx|none.
//
// Left-shift SOTA doctrine (structure encodes intelligence):
//
//	Burst ingest / continual upsert → retrieval_ready chunks + product structure
//	Gardener async (ENRICH=async)  → d2q / context / edges / dense warm
//	                                 bi-temporal claims + TemporalRelations
//	                                 lifecycle NREM/REM (dream)
//	Serve query (default PROD)     → HotLex+dense + path2/product structure SQL
//	                                 hops + hydrate + CE + ground (~10s)
//	QUALITY / research             → multi-arm + optional agentic (opt-in)
//	FS projection (OpenLocal)      → retained for agent ease-of-use
//
// Lean retrieve (serve / HotLex class — v5-gated, not always-max):
//
//  1. HotLex BM25 (if projected) + dense ANN in parallel (tight arm budgets)
//  2. Vocab recovery only when BM25 flat/weak (OUROBOROS_ERB_ALWAYS_RECOVERY=0 default)
//     + offline entity-catalog.gob hits + multi-query dense when QUALITY
//  3. Union CE heads → sibling hydrate
//  4. structure: pool-virtual + path2 entity/fact/rel SQL (+ project hop2) +
//     temporalRelationPassages
//  5. CE + coverage MMR + identifier floor → tight window
//  6. Supersession adjudicate only on near-dup groups or explicit conflict language
//
// Answer: type-aware synth → claim/quote ground → cite_atom_prune (answer-atom
// overlap) → dual-cite on contested · false-abstention / completeness retries ·
// map-reduce for project/completeness · info_not_found pure caveat.
//
// Stretch modules (2026-07-29): cite_atom_prune, conflict_adjudicate,
// entity_catalog(+offline gob), exhaustive_agentic, map_reduce_synth.
//
// smf-research Path-4 funnel (latency-tiered): lean/standard/expand budgets in
// smf_funnel.go — whole-doc hydrate, wide CE on expand, multi-gold coverage MMR,
// gated HyDE/doc2query/decompose recovery, facts inject, entity full fanout.
//
// Product write lifecycle: EnsureSchema → BurstUpsert (structure co-occur) →
// EnrichAfterIngest → gardener → cortex (claims+relations) → HotLex/dense warm.
//
// Env: NEON_*/QDRANT_*/COHERE_* for path2; OUROBOROS_ERB_HOTLEX_PATH; PROD vs QUALITY;
// OUROBOROS_ERB_BENCHMAX=1 (or OUROBOROS_ERB_BENCH_MAX=1) for an explicitly
// non-official pinned-brain ERB score chase. Official FTS remains live-bounded;
// OUROBOROS_BRAIN_ENRICH; OUROBOROS_BRAIN_GARDENER_AUTO; AS_OF/KNOWN_AT bi-temporal.
package hosted
