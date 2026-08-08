// Package chunking implements the versioned Ouroboros chunking policy
// (issue #332): a 500-token target with at least 50-token overlap baseline
// plus structure-aware and parent-child alternatives for code, tables,
// slides, and chat sources.
//
// The package is a deterministic leaf: it never touches storage or network.
// Output receipts carry tokenizer/version stamps, byte offsets into the
// exact chunked source string, parent identity, and content hashes so chunk
// identity is stable across rebuilds and verifiable by evaluation harnesses.
// Mapping receipts into hosted.ChunkWrite units is done by the caller so the
// existing ingestion contracts stay the single write path.
package chunking
