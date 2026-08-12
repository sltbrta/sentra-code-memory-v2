// Package codeserve is the multi-verb JSON protocol for product *code operator*
// tools (Phase 1 SCM CLI parity).
//
// # Scope
//
//   - Index / search / find-relevant / expand / impact / find-route / freshness
//   - Bounded read / imports / watch operator verbs
//   - Exact P5 via productsearch ProfileCodeExact
//   - Optional memory_ask for company residual (same process, different store)
//
// # Not in scope
//
// SCM *session product* (agent continuation packets, latent development-state
// memory). That is a different product class — see
// docs/specs/product/SCM-SESSION-PRODUCT.md and SCM-010.
//
// # Integration
//
// product-brain serve decodes one JSON object per stdin line and calls Handle.
// CLI remains the operator source of truth; serve is MCP-lite for agents.
//
// Latency: warm loads use OpenOrRefresh stamp path; "no_refresh": true loads
// an existing gob without crawling.
package codeserve
