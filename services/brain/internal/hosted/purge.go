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
// All five dense backends implement the purge port. Three are exercised
// directly here -- the in-memory store, the HNSW index, and the SQLite-backed
// local projection -- Postgres implements the same SQL as its writer, and the
// two remote ones are implemented against their documented APIs and exercised
// against fakes. None of that requires trusting an unverified call: the
// fan-out re-queries after the delete, so a wrong endpoint surfaces as a
// reported residual rather than as a silent success.
//
// A backend that cannot be reached at all is not wired, and the receipt names
// `vectors` as skipped rather than reading zero removals as an erasure.
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
// returns nil when the backend cannot be reached at all.
//
// Nil rather than an adapter that returns errors: the fan-out reports a nil
// port as a skipped substrate and refuses to call the purge complete, which is
// the right disposition for a store nothing can talk to. An adapter would
// instead report zero removals, which reads as "there was nothing to remove".
//
// Reachability is decided once, here, with an empty probe, rather than
// surfacing as a mid-purge error. A backend that answers an empty probe may
// still fail the real delete -- and that failure is reported as a residual by
// the verification pass, which is the point of having one.
func (c *Client) vectorPurger() deletion.VectorPurger {
	if c == nil || c.localDense == nil {
		return nil
	}
	if _, err := c.localDense.HasDocuments(nil); err != nil {
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
