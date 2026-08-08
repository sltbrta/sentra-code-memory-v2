// Package continual owns product continual ingestion: watch sources, compute
// deltas, write through hosted.Client (retrieval_ready), and enqueue async
// gardener jobs — never block lexical readiness on LLM enrichment.
//
// Stage localauthority ReconcileSource is retired as a product path (ADR 0022).
// Product continual is FS/jsonl (and code-watch via codecrawl) only.
package continual
