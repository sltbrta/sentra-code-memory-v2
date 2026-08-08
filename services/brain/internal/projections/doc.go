// Package projections persists rebuildable product-brain projections in SQLite.
//
// Ontology edges and dense vectors are derived artifacts (ADR 0002 / ADR 0020):
// they are not authority. Canonical evidence stays in the event/artifact stores;
// this package materializes generation-scoped graphs and embeddings so gardener
// and hybrid retrieval can restart without re-deriving from scratch.
//
// Uses modernc.org/sqlite (pure Go, no CGO), matching localstate and conversation.
package projections
