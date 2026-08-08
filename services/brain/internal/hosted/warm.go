package hosted

import (
	"context"
	"fmt"
	"strings"
)

// SidecarWrite is a gardener-style enrichment artifact (d2q / context).
// Product-owned: stored in product_sidecars (not SMF tables).
type SidecarWrite struct {
	DocumentID string
	Kind       string // d2q | context_header
	Text       string
}

// WarmSidecars upserts gardener warm artifacts into product_sidecars.
// Call after BurstUpsert when product owns the brain (Neon or memory).
func (c *Client) WarmSidecars(ctx context.Context, brainID string, items []SidecarWrite) (int, error) {
	if c == nil {
		return 0, fmt.Errorf("hosted: nil client")
	}
	brainID = strings.TrimSpace(brainID)
	if brainID == "" {
		return 0, fmt.Errorf("hosted: empty brain_id")
	}
	if len(items) == 0 {
		return 0, nil
	}
	var (
		n   int
		err error
	)
	if c.store != nil {
		if w, ok := c.store.(sidecarWarmer); ok {
			n, err = w.WarmSidecars(ctx, brainID, items)
			if err == nil && n > 0 {
				c.InvalidateQueryCache()
			}
			return n, err
		}
	}
	if c.db == nil {
		return 0, fmt.Errorf("hosted: no store for sidecars")
	}
	// Ensure product store
	if err := c.EnsureSchema(ctx); err != nil {
		return 0, err
	}
	if w, ok := c.store.(sidecarWarmer); ok {
		n, err = w.WarmSidecars(ctx, brainID, items)
		if err == nil && n > 0 {
			c.InvalidateQueryCache()
		}
		return n, err
	}
	return 0, fmt.Errorf("hosted: store does not support sidecars")
}

type sidecarWarmer interface {
	WarmSidecars(ctx context.Context, brainID string, items []SidecarWrite) (int, error)
}

// Ensure product schema includes sidecars (neonChunkStore).
func init() {
	productChunkSchemaStmts = append(productChunkSchemaStmts,
		`CREATE TABLE IF NOT EXISTS product_sidecars (
  brain_id TEXT NOT NULL,
  document_id TEXT NOT NULL,
  kind TEXT NOT NULL,
  text TEXT NOT NULL,
  PRIMARY KEY (brain_id, document_id, kind)
)`,
		`CREATE INDEX IF NOT EXISTS product_sidecars_brain_kind
  ON product_sidecars (brain_id, kind)`,
	)
}
