// Package dense stores document embeddings and performs nearest-neighbor search.
//
// v0 is a pure-Go in-memory store with brute-force cosine similarity (no CGO,
// no external ANN library). It is the product dense projection used by hybrid
// retrieval (BM25 + dense → RRF → CE), not a full-corpus external vector DB.
//
// Product path only (ADR 0020). Eval harnesses must not reimplement dense search.
package dense
