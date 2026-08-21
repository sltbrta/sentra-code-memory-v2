package hosted

import (
	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/deletion"
)

// PurgeDocuments deletes documents from every content-bearing substrate this
// client has, and returns a verified receipt.
//
// Deletion previously flipped a manifest to immediate-deny and asked the
// ArtifactVault to drop the bytes. Nothing removed the projections that
// actually answer a query, so a deleted document kept being retrieved,
// ranked and cited.
//
// The dense backend is deliberately not wired here. There are five
// implementations behind denseBackend -- SQLite, Postgres, FAISS, HNSW and
// Qdrant -- and none exposes a delete; adding one to each without being able
// to exercise Postgres or Qdrant would be shipping an erasure path that has
// never run. The receipt names `vectors` as skipped and refuses to report
// VerifiedComplete, which is the honest answer: this purge removes the
// document from the corpus, the lexical index, the cortex and the history, and
// says plainly that the vector store still holds its embeddings.
func (c *Client) PurgeDocuments(brainID string, docIDs []string) (deletion.PurgeReceipt, error) {
	if c == nil {
		return deletion.PurgeReceipt{}, deletion.ErrNoSubstrates
	}
	if brainID == "" {
		brainID = c.cfg.BrainID
	}
	substrates := deletion.Substrates{BrainID: brainID}
	if purger, ok := c.store.(deletion.CorpusPurger); ok {
		substrates.Corpus = purger
	}
	if c.Mem != nil {
		substrates.Cortex = c.Mem
		substrates.History = c.Mem
	}
	return deletion.Purge(substrates, docIDs)
}
