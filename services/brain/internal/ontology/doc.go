// Package ontology defines the typed enterprise memory graph for the product brain.
//
// Ontology is data, not hard-coded product logic: entity kinds, relation kinds,
// and edges are versioned projections rebuildable from evidence generations.
// Query may traverse entitled edges; gardener may propose edges; only
// deterministic admission and ACL decide what is visible.
//
// This package is the product spine for multi-hop / company-doc structure
// (ADR 0020). It does not open sockets or call LLMs.
package ontology
