package rerank

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"
)

// The reranker clipped each document with `d[:1500]`, a byte offset. That
// lands mid-rune on any non-ASCII input, and the destination is a model
// provider's JSON body -- encoding/json substitutes U+FFFD for invalid bytes,
// so the last token of every truncated document was corrupted before it was
// scored. The pass that replaced about a dozen of these helpers with
// textbound did not reach this one.

func TestClippedDocumentsStayValidUTF8(t *testing.T) {
	// A single leading ASCII byte, then three-byte runes.
	//
	// The offset matters: 1500 is divisible by 2, 3 and 4, so a document of
	// uniform multi-byte runes happens to cut exactly on a boundary and proves
	// nothing. The first draft of this test did that and skipped itself. One
	// byte of shift puts the limit one byte inside a rune.
	doc := "p" + strings.Repeat("界", 2000)
	if utf8.RuneLen('界') != 3 {
		t.Skip("fixture assumes a three-byte rune")
	}
	if (zeroEntropyMaxDocChars-1)%3 == 0 {
		t.Skip("the limit lands on a rune boundary for this fixture")
	}

	clipped := clipRerankDocuments([]string{doc})
	if len(clipped) != 1 {
		t.Fatalf("want 1 document, got %d", len(clipped))
	}
	if !utf8.ValidString(clipped[0]) {
		t.Fatal("a clipped document is not valid UTF-8: the provider's JSON " +
			"encoder replaces the fragment with U+FFFD, so the last token of " +
			"every truncated document is corrupted before it is scored")
	}
	if len(clipped[0]) > zeroEntropyMaxDocChars {
		t.Fatalf("clipped to %d bytes, over the %d limit", len(clipped[0]), zeroEntropyMaxDocChars)
	}

	// The corruption is only visible once it is encoded, which is what the
	// document is for.
	encoded, err := json.Marshal(clipped[0])
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), `�`) {
		t.Fatalf("the encoded document carries a replacement character: %s",
			string(encoded)[len(encoded)-40:])
	}
}

func TestShortDocumentsAreUntouched(t *testing.T) {
	doc := "package main // 界"
	clipped := clipRerankDocuments([]string{doc})
	if clipped[0] != doc {
		t.Fatalf("a document inside the limit was altered: %q", clipped[0])
	}
}

func TestAsciiClippingIsUnchanged(t *testing.T) {
	doc := strings.Repeat("a", 2000)
	clipped := clipRerankDocuments([]string{doc})
	if len(clipped[0]) != zeroEntropyMaxDocChars {
		t.Fatalf("ASCII clipped to %d, want %d", len(clipped[0]), zeroEntropyMaxDocChars)
	}
}
