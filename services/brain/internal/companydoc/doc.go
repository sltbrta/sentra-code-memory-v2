// Package companydoc imports company document batches for the product brain.
//
// LiveCorpus is an in-process bag-of-words fixture for unit tests and tiny
// synthetic corpora. Full-enterprise retrieval (EnterpriseRAG, production
// company docs at scale) uses package hosted (one residual Client; Neon + Qdrant
// are substrates, not a second product), not LiveCorpus.
package companydoc
