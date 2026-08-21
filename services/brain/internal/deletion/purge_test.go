package deletion

import (
	"errors"
	"testing"
)

// internal/deletion had no test file at all, which is part of why nothing
// noticed that deleting content left it answering searches: Tombstone flips a
// manifest to immediate-deny and schedules a purge job, and neither it nor
// CompletePurge touches a single surface that answers a query.

// fakeCorpus is a substrate that deletes what it is asked to.
type fakeCorpus struct {
	docs    map[string]bool
	deleted int
}

func newFakeCorpus(ids ...string) *fakeCorpus {
	docs := map[string]bool{}
	for _, id := range ids {
		docs[id] = true
	}
	return &fakeCorpus{docs: docs}
}

func (c *fakeCorpus) DeleteDocuments(_ string, docIDs []string) int {
	n := 0
	for _, id := range docIDs {
		if c.docs[id] {
			delete(c.docs, id)
			n++
		}
	}
	c.deleted += n
	return n
}

func (c *fakeCorpus) DocumentIDs(string) []string {
	out := make([]string, 0, len(c.docs))
	for id := range c.docs {
		out = append(out, id)
	}
	return out
}

// leakyCorpus reports a delete count but keeps the documents, which is exactly
// the shape of the defect: a substrate that says it removed something and did
// not. Only a second look catches it.
type leakyCorpus struct{ *fakeCorpus }

func (c leakyCorpus) DeleteDocuments(_ string, docIDs []string) int { return len(docIDs) }

type fakeVectors struct{ vectors map[string]bool }

func (v *fakeVectors) DeleteDocumentVectors(docIDs []string) int {
	n := 0
	for _, id := range docIDs {
		if v.vectors[id] {
			delete(v.vectors, id)
			n++
		}
	}
	return n
}

func (v *fakeVectors) HasDocumentVectors(docIDs []string) []string {
	var out []string
	for _, id := range docIDs {
		if v.vectors[id] {
			out = append(out, id)
		}
	}
	return out
}

type fakeCortex struct {
	docs map[string]bool
	err  error
}

func (c *fakeCortex) PurgeDocuments(docIDs []string) (int, error) {
	if c.err != nil {
		return 0, c.err
	}
	n := 0
	for _, id := range docIDs {
		if c.docs[id] {
			delete(c.docs, id)
			n++
		}
	}
	return n, nil
}

func (c *fakeCortex) ResidualDocuments(docIDs []string) []string {
	var out []string
	for _, id := range docIDs {
		if c.docs[id] {
			out = append(out, id)
		}
	}
	return out
}

type fakeHistory struct{ docs map[string]bool }

func (h *fakeHistory) PurgeHistory(docIDs []string) (int, error) {
	n := 0
	for _, id := range docIDs {
		if h.docs[id] {
			delete(h.docs, id)
			n++
		}
	}
	return n, nil
}

func (h *fakeHistory) ResidualHistory(docIDs []string) []string {
	var out []string
	for _, id := range docIDs {
		if h.docs[id] {
			out = append(out, id)
		}
	}
	return out
}

func allSubstrates() Substrates {
	return Substrates{
		BrainID: "brain-1",
		Corpus:  newFakeCorpus("drop", "keep"),
		Vectors: &fakeVectors{vectors: map[string]bool{"drop": true, "keep": true}},
		Cortex:  &fakeCortex{docs: map[string]bool{"drop": true, "keep": true}},
		History: &fakeHistory{docs: map[string]bool{"drop": true, "keep": true}},
	}
}

func TestPurgeReachesEverySubstrate(t *testing.T) {
	substrates := allSubstrates()
	receipt, err := Purge(substrates, []string{"drop"})
	if err != nil {
		t.Fatal(err)
	}
	if !receipt.VerifiedComplete {
		t.Fatalf("purge not verified complete: %+v", receipt)
	}
	for _, substrate := range purgeSubstrates {
		if receipt.Purged[substrate] != 1 {
			t.Errorf("%s purged %d, want 1: a deletion that misses a substrate "+
				"leaves the document answering queries there",
				substrate, receipt.Purged[substrate])
		}
	}
	if len(receipt.Leaks) != 0 {
		t.Fatalf("leaks reported after a complete purge: %+v", receipt.Leaks)
	}
	// The document that was not asked for must survive everywhere.
	if len(substrates.Cortex.ResidualDocuments([]string{"keep"})) != 1 {
		t.Fatal("purging one document removed another")
	}
}

// TestPurgeReportsALeakThatADeleteCountHides is why verification is a second
// pass rather than a sum of the counts. A substrate can report that it removed
// a document and still hold it.
func TestPurgeReportsALeakThatADeleteCountHides(t *testing.T) {
	substrates := allSubstrates()
	substrates.Corpus = leakyCorpus{fakeCorpus: newFakeCorpus("drop")}

	receipt, err := Purge(substrates, []string{"drop"})
	if err != nil {
		t.Fatal(err)
	}
	if receipt.Purged["corpus"] != 1 {
		t.Fatalf("the fixture must report a delete: %+v", receipt.Purged)
	}
	if receipt.VerifiedComplete {
		t.Fatal("a purge that left the document in the corpus reported complete: " +
			"the receipt is summing delete counts rather than looking again")
	}
	if got := receipt.Leaks["corpus"]; len(got) != 1 || got[0] != "drop" {
		t.Fatalf("corpus leak not reported: %+v", receipt.Leaks)
	}
}

// TestAnUnwiredSubstrateIsNamedAndBlocksCompleteness is the honesty property.
// Three substrates out of four is not a deletion, and a receipt that said
// "complete" because the fourth was nil would be the same overclaiming as the
// manifest flip this replaces.
func TestAnUnwiredSubstrateIsNamedAndBlocksCompleteness(t *testing.T) {
	substrates := allSubstrates()
	substrates.Vectors = nil

	receipt, err := Purge(substrates, []string{"drop"})
	if err != nil {
		t.Fatal(err)
	}
	if receipt.VerifiedComplete {
		t.Fatal("a purge that never reached the vector store reported complete")
	}
	if len(receipt.Skipped) != 1 || receipt.Skipped[0] != "vectors" {
		t.Fatalf("the unwired substrate was not named: %+v", receipt.Skipped)
	}
	// The substrates that were wired must still have been purged.
	if receipt.Purged["corpus"] != 1 || receipt.Purged["cortex"] != 1 {
		t.Fatalf("wired substrates were not purged: %+v", receipt.Purged)
	}
}

func TestPurgeWithNoSubstratesIsAnError(t *testing.T) {
	_, err := Purge(Substrates{BrainID: "brain-1"}, []string{"drop"})
	if !errors.Is(err, ErrNoSubstrates) {
		t.Fatalf("want ErrNoSubstrates, got %v", err)
	}
}

func TestPurgeRejectsAnEmptyRequest(t *testing.T) {
	if _, err := Purge(allSubstrates(), nil); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("want ErrInvalidTransition, got %v", err)
	}
	if _, err := Purge(allSubstrates(), []string{"", "  "}); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("blank ids were accepted as a purge target: %v", err)
	}
}

// TestPurgeReceiptIsAFunctionOfTheRequestedSet keeps a receipt comparable
// across runs: the same set asked for in a different order is the same purge.
func TestPurgeReceiptIsAFunctionOfTheRequestedSet(t *testing.T) {
	first, err := Purge(allSubstrates(), []string{"drop", "keep", "drop"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := Purge(allSubstrates(), []string{"keep", "drop"})
	if err != nil {
		t.Fatal(err)
	}
	if len(first.DocumentIDs) != 2 {
		t.Fatalf("duplicate not collapsed: %+v", first.DocumentIDs)
	}
	for i := range first.DocumentIDs {
		if first.DocumentIDs[i] != second.DocumentIDs[i] {
			t.Fatalf("receipt depends on request order: %v vs %v",
				first.DocumentIDs, second.DocumentIDs)
		}
	}
}

// TestACortexFailureIsNotReportedAsASuccessfulPurge keeps a substrate error
// from being swallowed into a receipt.
func TestACortexFailureIsNotReportedAsASuccessfulPurge(t *testing.T) {
	substrates := allSubstrates()
	substrates.Cortex = &fakeCortex{docs: map[string]bool{"drop": true}, err: errors.New("disk full")}

	receipt, err := Purge(substrates, []string{"drop"})
	if err == nil {
		t.Fatal("a failing substrate reported a successful purge")
	}
	if receipt.VerifiedComplete {
		t.Fatal("a failed purge is marked complete")
	}
}
