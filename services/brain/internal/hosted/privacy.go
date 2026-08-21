package hosted

import (
	"context"
	"fmt"
	"sync"

	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/contentprivacy"
)

// contentprivacy ran nowhere.
//
// It is the only PII and secret redaction in the product, and no ingest path
// called it, so emails, national identifiers, card numbers, bearer tokens and
// private keys in ingested content were written verbatim to chunks.jsonl, the
// HotLex index, the dense store and the memory cortex. guard.go documented
// guarantees that nothing provided.
//
// Redaction happens at the document boundary, before DocumentsToChunks, rather
// than at the chunk boundary. Two reasons, and the second is the one that
// matters:
//
//   - A secret split across a chunk boundary is invisible to a per-chunk
//     detector. The private-key rule already had to be widened because
//     chunking cut the END marker off; redacting per chunk would reintroduce
//     that class of miss for every other pattern.
//   - BurstIngestLocal fans the original documents out to seedMemoryAfterIngest
//     and seedDenseAfterIngest, not the chunks. Redacting only the chunks would
//     have sanitised the corpus while the cortex and the vector store kept the
//     raw text -- which reads as redaction and is not.
//
// The publication goes through ProductionProjectionAdapter rather than calling
// Guard.Admit and using the result, so the redacted text a caller receives can
// only have come from Guard's validated admission path. That is the whole
// point of that type: it is the only call from raw Input to a sink, and its
// fields are private so a caller cannot substitute its own text.

// PrivacyOutcome reports what redaction did to one ingest.
type PrivacyOutcome struct {
	// Admitted is the documents that may be indexed, with sensitive spans
	// already redacted.
	Admitted []LocalDocument
	// Withheld names documents the policy tombstoned or quarantined. They are
	// not indexed and their ids are reported rather than silently dropped.
	Withheld map[string]string
	// Redacted counts documents whose text was changed.
	Redacted int
}

// WithContentPrivacy installs the redaction guard on this client.
//
// Ingest paths route documents through it before anything derived from them is
// built. A nil guard leaves the client unguarded, which is what every
// deployment was before this existed -- so it is a state a composition has to
// choose rather than fall into.
func (c *Client) WithContentPrivacy(guard *contentprivacy.Guard, scope contentprivacy.Scope) *Client {
	if c == nil {
		return c
	}
	c.privacyGuard = guard
	c.privacyScope = scope
	return c
}

// redactDocuments runs documents through the guard, returning what may be
// indexed. An unguarded client returns its input unchanged.
func (c *Client) redactDocuments(ctx context.Context, docs []LocalDocument) (PrivacyOutcome, error) {
	if c == nil || c.privacyGuard == nil || len(docs) == 0 {
		return PrivacyOutcome{Admitted: docs}, nil
	}

	sink := &projectionSink{projections: map[string]contentprivacy.Projection{}}
	adapter, err := contentprivacy.NewProductionProjectionAdapter(c.privacyGuard, sink)
	if err != nil {
		return PrivacyOutcome{}, fmt.Errorf("hosted: compose content privacy: %w", err)
	}

	outcome := PrivacyOutcome{
		Admitted: make([]LocalDocument, 0, len(docs)),
		Withheld: map[string]string{},
	}
	tenant := c.cfg.BrainID
	for _, doc := range docs {
		decision, err := adapter.AdmitAndPublish(ctx, contentprivacy.Input{
			TenantID: tenant, ID: doc.ID, Scope: c.privacyScope, Content: doc.Text,
		})
		if err != nil {
			// Fail closed. A document that could not be inspected is not
			// indexed: admitting it would publish exactly the content the
			// guard exists to withhold.
			outcome.Withheld[doc.ID] = "inspection_failed"
			continue
		}
		projection, published := sink.take(doc.ID)
		if !published {
			outcome.Withheld[doc.ID] = string(decision.Status)
			continue
		}
		if projection.IndexText != doc.Text {
			outcome.Redacted++
		}
		redacted := doc
		redacted.Text = projection.IndexText
		outcome.Admitted = append(outcome.Admitted, redacted)
	}
	if len(outcome.Withheld) == 0 {
		outcome.Withheld = nil
	}
	return outcome, nil
}

// projectionSink captures what the adapter publishes.
//
// It is a sink rather than a store because the guard's contract is that a
// projection reaching a publisher has been through its validated path; what
// this deployment does with it afterwards -- chunk it, index it, embed it --
// is the ordinary ingest pipeline, which now only ever sees redacted text.
type projectionSink struct {
	mu          sync.Mutex
	projections map[string]contentprivacy.Projection
}

func (s *projectionSink) PublishProjection(_ context.Context, projection contentprivacy.Projection) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.projections[projection.ID] = projection
	return nil
}

func (s *projectionSink) take(id string) (contentprivacy.Projection, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	projection, ok := s.projections[id]
	delete(s.projections, id)
	return projection, ok
}
