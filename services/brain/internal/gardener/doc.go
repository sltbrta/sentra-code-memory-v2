// Package gardener implements async enrichment and lifecycle consolidation
// after retrieval_ready.
//
// # Enrichment (default product path)
//
//   - Admit publishes a generation that is already queryable (lexical).
//   - Jobs (doc2query, edges, context_header, dense, …) run under budgets.
//   - Never mutate primary evidence — only projections / sidecars.
//   - Durable queue: gardener.db (SQLiteQueue) on local_fs brains.
//
// # Post-wave cortex (product hook, not a queue job)
//
// After RunGardenerWave (and sync EnrichAfterIngest) drains enrich jobs,
// product-hosted calls memory.RunCortexMaintenance when Mem is attached:
// claim extract → SeedRelationsFromClaims (TemporalRelations) → prose edges →
// PageIndex → global PageRank → community/RAPTOR. Ingest hot path stays LIGHT
// (utility + texts + episode only). Lean serve reads ExpandRelationDocuments.
//
// # Lifecycle (Phase 3, CLI --lifecycle)
//
// C1 predict-calibrate may skip heavy work; otherwise product post-wave closes
// the memory-cortex loops (order matters):
//
//  1. RunCortexMaintenance (extract→relations/edges/pageindex/PR) if not warm
//  2. Utility half-life decay (OnUtilityDecay → ranking)
//  3. NREM — low-utility quarantine on cortex (RunNREM; no chunk delete)
//  4. REM — opt-in deterministic re-extract + reseed TemporalRelations
//     (EnableREM + --rem or OUROBOROS_BRAIN_REM=1; quarantine low-conf)
//  5. RAPTOR refresh after NREM (community nodes preserved)
//  6. Claim-linked edges + C5-light hypothesize / weak prune
//  7. Episode reseg (LifecycleResegment)
//
// Queue workers remain deterministic receipts; the product-brain CLI owns the
// cortex mutations so digests stay stable (GDN-002).
//
// CLI: product-brain gardener --dir … [--once | --lifecycle] [--rem]
//
//	[--lifecycle-interval 30m]  (loop mode periodic lifecycle tick)
//
// Env:  OUROBOROS_BRAIN_GARDENER_AUTO=1 starts background drain after OpenLocal
package gardener
