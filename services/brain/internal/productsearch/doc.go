// Package productsearch is the unified product search/ask facade.
//
// ONE product brain; profiles are backends, not products (ADR 0021):
//
//	local  → hosted.OpenLocal (durable FS projection)
//	hosted → path2 / product Neon from env (same hosted.Client API)
//	code   → codecrawl multi-crawler index + symbol hop
//
// Local and hosted are interchangeable store adapters of hosted.Client.
// Dual Python product engines live under archive/2026-07-product-brain-consolidation/.
package productsearch
