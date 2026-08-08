// Package codecrawl provides a parallel multi-crawler code index for FTS-like
// search over a source tree, plus a file-incremental symbol graph (stack-graph
// inspired: file-disjoint defs/refs, query-time hop over shared names).
//
// Designed as a giant search problem: multi-worker crawl = sharded index
// construction; Search/SearchOpts = multi-arm retrieve (lexical + optional hop).
// Product code path aims for ≥ SCM function with superior multi-crawler wall time.
package codecrawl
