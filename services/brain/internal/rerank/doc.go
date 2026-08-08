// Package rerank provides embedding and cross-encoder rerank clients for hybrid
// retrieval on the product brain path (dense + CE after lexical/RRF).
//
// Interfaces:
//   - Embedder: batch text → [][]float32 (OpenAI-compatible HTTP or wrappers)
//   - Reranker: query + docs → ranked indices (ZeroEntropy-style HTTP or lexical)
//   - IdentityProvider: optional, declares the embedding identity so a
//     wrapper can scope cache entries by model/dimension/normalization.
//
// HTTP implementations read OPENAI_API_KEY / ZEROENTROPY_API_KEY when constructed
// via FromEnv helpers. LexicalReranker is always available and needs no network.
// CachedEmbedder is a legacy unbounded text-only cache. EmbedCache is the
// production TTL+LRU cache: keys cover input type, tenant, generation, and
// a sha256 text digest; entries additionally carry their embedding identity
// which is verified on lookup so a model, dimension, or normalization change
// never silently reuses an incompatible vector (issue #329).
//
// Product path only (ADR 0020). Unit tests never hit the network.
package rerank
