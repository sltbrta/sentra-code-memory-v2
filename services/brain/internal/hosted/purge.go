package hosted

import (
	"errors"

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
// The dense backend is wired when it can verifiably purge. Three of the five
// implementations can and are exercised here -- the in-memory store, the HNSW
// index, and the SQLite-backed local projection -- and Postgres implements the
// same SQL as its writer. FAISS and Qdrant return ErrPurgeUnsupported: they
// are remote services whose delete surfaces this repository cannot exercise,
// and an unverified HTTP call reported as an erasure is worse than a named
// gap. Against those two the receipt still names `vectors` as skipped and
// still refuses VerifiedComplete, which is the honest answer.
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
	if purger := c.vectorPurger(); purger != nil {
		substrates.Vectors = purger
	}
	return deletion.Purge(substrates, docIDs)
}

// vectorPurger adapts this client's dense backend to the purge port, or
// returns nil when the backend cannot verifiably delete.
//
// Nil rather than an adapter that returns errors: the fan-out reports a nil
// port as a skipped substrate and refuses to call the purge complete, which is
// exactly the disposition an unsupported backend deserves. An adapter that
// swallowed ErrPurgeUnsupported would report zero removals as a success.
func (c *Client) vectorPurger() deletion.VectorPurger {
	if c == nil || c.localDense == nil {
		return nil
	}
	// A backend that refuses one probe refuses all of them, so the capability
	// is decided once, here, rather than surfacing as a mid-purge error.
	if _, err := c.localDense.HasDocuments(nil); errors.Is(err, ErrPurgeUnsupported) {
		return nil
	}
	return denseVectorPurger{backend: c.localDense}
}

// denseVectorPurger adapts denseBackend to deletion.VectorPurger.
type denseVectorPurger struct{ backend denseBackend }

func (p denseVectorPurger) DeleteDocumentVectors(docIDs []string) int {
	removed, err := p.backend.DeleteDocuments(docIDs)
	if err != nil {
		// A failed delete must not look like "there was nothing to remove".
		// The verification pass below reports the survivors, and a survivor is
		// what turns this into an incomplete purge.
		return removed
	}
	return removed
}

func (p denseVectorPurger) HasDocumentVectors(docIDs []string) []string {
	found, err := p.backend.HasDocuments(docIDs)
	if err != nil {
		// Unable to verify is not the same as verified empty. Reporting every
		// id as residual makes the purge incomplete, which is the fail-closed
		// reading.
		return docIDs
	}
	return found
}
