package companydoc

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"time"
)

// SourceKind classifies a company-document source feed.
type SourceKind string

const (
	SourceInline  SourceKind = "inline"
	SourceJSONL   SourceKind = "jsonl"
	SourceFTSDump SourceKind = "fts_sqlite_export"
	SourceERBVol  SourceKind = "erb_volume_import"
)

// Document is one immutable text unit for a generation.
type Document struct {
	ID       string
	Title    string
	Text     string
	Source   string // connector or path hint (non-secret)
	MimeType string
	// SourceTypes free-form tags (confluence, slack, …) for authority ranking.
	SourceTypes []string
	ObservedAt  time.Time
}

// Digest returns sha256 hex of id + text (content identity).
func (d Document) Digest() string {
	h := sha256.New()
	_, _ = h.Write([]byte(d.ID))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(d.Text))
	return hex.EncodeToString(h.Sum(nil))
}

// Batch is a set of documents admitted together as one generation candidate.
type Batch struct {
	SourceID     string
	GenerationID string
	Kind         SourceKind
	Documents    []Document
	CreatedAt    time.Time
}

// ValidateBatch checks structural admit rules (no ACL — caller authorizes).
func ValidateBatch(b Batch) error {
	if b.SourceID == "" || b.GenerationID == "" {
		return fmt.Errorf("companydoc: source and generation required")
	}
	if len(b.Documents) == 0 {
		return fmt.Errorf("companydoc: empty batch")
	}
	seen := map[string]struct{}{}
	for i, d := range b.Documents {
		if d.ID == "" {
			return fmt.Errorf("companydoc: document %d missing id", i)
		}
		if _, ok := seen[d.ID]; ok {
			return fmt.Errorf("companydoc: duplicate document id %s", d.ID)
		}
		seen[d.ID] = struct{}{}
		if d.Text == "" {
			return fmt.Errorf("companydoc: document %s empty text", d.ID)
		}
	}
	return nil
}

// TextMap returns id → text for gardener / ontology consumers.
func TextMap(docs []Document) map[string]string {
	out := make(map[string]string, len(docs))
	for _, d := range docs {
		out[d.ID] = d.Text
	}
	return out
}
