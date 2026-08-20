package hosted

import (
	"fmt"
	"testing"
)

// Compaction rebuilt the index by replaying addChunk over the surviving docs,
// and addChunk returns early on empty text. StripStoredText -- the index-only
// serving shape used for the Modal/hotlex snapshot -- sets every doc's Text to
// "". So once tombstones crossed 25%, the rebuild dropped every live document
// and left N=0: the whole lexical serving index went empty, with no error, and
// every query returned nothing from then on.

func TestCompactionKeepsLiveDocumentsInAStrippedIndex(t *testing.T) {
	h := NewHotLex("brain-1")
	const total = 200
	for i := 0; i < total; i++ {
		h.AddChunkBulk(fmt.Sprintf("chunk-%03d", i), fmt.Sprintf("doc-%03d", i),
			"alpha beta gamma delta", "file://x", true)
	}
	// Index-only shape: text is dropped, postings remain.
	h.StripStoredText()

	before := h.Search("alpha", 10)
	if len(before) == 0 {
		t.Fatal("stripped index should still answer from its postings")
	}

	// Push tombstones past the 25% compaction threshold.
	for i := 0; i < total/2; i++ {
		h.RemoveDocument(fmt.Sprintf("doc-%03d", i))
	}

	after := h.Search("alpha", 10)
	if len(after) == 0 {
		t.Fatal("compaction emptied a stripped index: every surviving document was dropped")
	}
	if n := h.Len(); n == 0 {
		t.Fatalf("Len = %d after compaction, want the surviving half", n)
	}
}

// TestCompactionStillCompactsATextBearingIndex keeps the guard from disabling
// compaction outright.
func TestCompactionStillCompactsATextBearingIndex(t *testing.T) {
	h := NewHotLex("brain-1")
	const total = 200
	for i := 0; i < total; i++ {
		h.AddChunkBulk(fmt.Sprintf("chunk-%03d", i), fmt.Sprintf("doc-%03d", i),
			"alpha beta gamma delta", "file://x", true)
	}
	for i := 0; i < total/2; i++ {
		h.RemoveDocument(fmt.Sprintf("doc-%03d", i))
	}
	if got := h.Len(); got != total/2 {
		t.Fatalf("Len = %d, want %d live documents", got, total/2)
	}
	if len(h.Search("alpha", 10)) == 0 {
		t.Fatal("surviving documents should still be searchable")
	}
}
