package deletion

import (
	"errors"
	"fmt"
	"sort"
	"strings"
)

// Deleting content left it answering searches.
//
// Tombstone flips a manifest to immediate-deny and schedules a purge job, and
// CompletePurge records that the ArtifactVault disposed of the bytes. Neither
// touches the surfaces that actually answer a query: the corpus and its
// sidecars, the lexical index, the dense vectors, the memory cortex's document
// bodies and edges, the session history, and the query log -- which records the
// document ids a question was answered from. A deletion therefore removed the
// object and left every projection of it in place.
//
// orgscope holds the only complete, leak-verified erasure in the repository,
// but it erases its own in-memory model rather than the product's substrates,
// and it has no caller. What is reused here is its discipline rather than its
// code: name the exact coverage, count what each substrate purged, and then go
// back and look for what survived instead of assuming the deletes worked.
//
// Purge is deliberately a fan-out over small ports rather than a method on any
// one store. The substrates do not share an interface and are composed
// differently in the local and hosted paths, and a purge that silently skipped
// a substrate because it was not wired is the failure being removed.

// PurgeCoverage names the exact substrate set VerifiedComplete is a claim
// about. It excludes retained audit metadata, receipts, and any object store,
// whose disposition is the ArtifactVault's to report.
const PurgeCoverage = "product_content_projections_v1"

// ErrNoSubstrates reports a purge with nothing wired to purge from. It is an
// error rather than an empty success: a fan-out that reaches no substrate must
// not report that content is gone.
var ErrNoSubstrates = errors.New("deletion: no content substrates configured")

// CorpusPurger removes documents from the durable corpus, its sidecars and the
// lexical index built from them.
type CorpusPurger interface {
	DeleteDocuments(brainID string, docIDs []string) int
	DocumentIDs(brainID string) []string
}

// VectorPurger removes a document's vectors from the dense store. Vectors are
// keyed per chunk, so the purger is asked for a document's chunk ids.
type VectorPurger interface {
	DeleteDocumentVectors(docIDs []string) int
	HasDocumentVectors(docIDs []string) []string
}

// CortexPurger removes a document's bodies, edges, claims and derived rank
// entries from the memory cortex.
type CortexPurger interface {
	PurgeDocuments(docIDs []string) (int, error)
	ResidualDocuments(docIDs []string) []string
}

// HistoryPurger removes a document from session history and the query log,
// which records the document ids each question was answered from.
type HistoryPurger interface {
	PurgeHistory(docIDs []string) (int, error)
	ResidualHistory(docIDs []string) []string
}

// Substrates is the set of content-bearing surfaces a purge fans out to. A nil
// field is a substrate this deployment does not have; every non-nil one is
// purged and then verified.
type Substrates struct {
	BrainID string
	Corpus  CorpusPurger
	Vectors VectorPurger
	Cortex  CortexPurger
	History HistoryPurger
}

// PurgeReceipt reports one fan-out.
//
// Purged counts what each substrate removed; Leaks names what was still found
// afterwards; Skipped names substrates that were not wired at all.
//
// VerifiedComplete requires both no leaks *and* nothing skipped. A purge that
// reached three of four substrates has not deleted the content, and a receipt
// that said "complete" because the fourth was nil would be the same
// overclaiming as the manifest flip that started this: a status that reads as
// a guarantee and is not one.
type PurgeReceipt struct {
	Coverage         string              `json:"coverage"`
	DocumentIDs      []string            `json:"document_ids"`
	Purged           map[string]int      `json:"purged"`
	Leaks            map[string][]string `json:"leaks,omitempty"`
	Skipped          []string            `json:"skipped,omitempty"`
	VerifiedComplete bool                `json:"verified_complete"`
}

// purgeSubstrates is the exact substrate set PurgeCoverage names. A nil port
// for any of them is recorded as skipped rather than passed over.
var purgeSubstrates = []string{"corpus", "vectors", "cortex", "history"}

// Purge removes docIDs from every configured substrate and then verifies that
// none of them can still be found.
//
// Verification is a second pass rather than a sum of the delete counts. A
// count says how many rows a delete statement matched; it cannot say whether
// the document survives somewhere the delete did not look, which is the whole
// shape of this defect.
func Purge(substrates Substrates, docIDs []string) (PurgeReceipt, error) {
	ids := normalizeIDs(docIDs)
	if len(ids) == 0 {
		return PurgeReceipt{}, fmt.Errorf("deletion: %w", ErrInvalidTransition)
	}
	receipt := PurgeReceipt{
		Coverage: PurgeCoverage, DocumentIDs: ids, Purged: map[string]int{},
	}

	if substrates.Corpus != nil {
		receipt.Purged["corpus"] = substrates.Corpus.DeleteDocuments(substrates.BrainID, ids)
	}
	if substrates.Vectors != nil {
		receipt.Purged["vectors"] = substrates.Vectors.DeleteDocumentVectors(ids)
	}
	if substrates.Cortex != nil {
		n, err := substrates.Cortex.PurgeDocuments(ids)
		if err != nil {
			return receipt, fmt.Errorf("deletion: purge cortex: %w", err)
		}
		receipt.Purged["cortex"] = n
	}
	if substrates.History != nil {
		n, err := substrates.History.PurgeHistory(ids)
		if err != nil {
			return receipt, fmt.Errorf("deletion: purge history: %w", err)
		}
		receipt.Purged["history"] = n
	}

	receipt.Skipped = substrates.skipped()
	if len(receipt.Skipped) == len(purgeSubstrates) {
		return receipt, ErrNoSubstrates
	}

	receipt.Leaks = verifyPurge(substrates, ids)
	receipt.VerifiedComplete = len(receipt.Leaks) == 0 && len(receipt.Skipped) == 0
	return receipt, nil
}

// skipped names the substrates in PurgeCoverage that this fan-out was not
// given a port for.
func (s Substrates) skipped() []string {
	present := map[string]bool{
		"corpus":  s.Corpus != nil,
		"vectors": s.Vectors != nil,
		"cortex":  s.Cortex != nil,
		"history": s.History != nil,
	}
	var out []string
	for _, name := range purgeSubstrates {
		if !present[name] {
			out = append(out, name)
		}
	}
	return out
}

// verifyPurge asks each substrate what it still holds. An id that comes back
// is a leak, whatever the delete counts said.
func verifyPurge(substrates Substrates, ids []string) map[string][]string {
	leaks := map[string][]string{}
	record := func(substrate string, residual []string) {
		if len(residual) == 0 {
			return
		}
		sort.Strings(residual)
		leaks[substrate] = residual
	}
	if substrates.Corpus != nil {
		wanted := make(map[string]struct{}, len(ids))
		for _, id := range ids {
			wanted[id] = struct{}{}
		}
		var residual []string
		for _, remaining := range substrates.Corpus.DocumentIDs(substrates.BrainID) {
			if _, ok := wanted[remaining]; ok {
				residual = append(residual, remaining)
			}
		}
		record("corpus", residual)
	}
	if substrates.Vectors != nil {
		record("vectors", substrates.Vectors.HasDocumentVectors(ids))
	}
	if substrates.Cortex != nil {
		record("cortex", substrates.Cortex.ResidualDocuments(ids))
	}
	if substrates.History != nil {
		record("history", substrates.History.ResidualHistory(ids))
	}
	if len(leaks) == 0 {
		return nil
	}
	return leaks
}

// normalizeIDs sorts and de-duplicates, so a receipt is a function of the set
// asked for rather than of the order it was written in.
func normalizeIDs(docIDs []string) []string {
	seen := make(map[string]struct{}, len(docIDs))
	out := make([]string, 0, len(docIDs))
	for _, id := range docIDs {
		if id = strings.TrimSpace(id); id == "" {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}
