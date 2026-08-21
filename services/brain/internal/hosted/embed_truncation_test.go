package hosted

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"unicode/utf8"
)

// The embed request bounded its input with `text = text[:8000]`, a byte offset
// that lands mid-rune on any non-ASCII text.
//
// It is one of sixteen inline truncations a fresh sweep found across the
// packages the audit ledger never triaged: the repository-wide pass that
// replaced about a dozen private truncate *helpers* with textbound never
// reached the ones written directly at a call site.
//
// This is the site where the consequence is visible rather than theoretical.
// The truncated text goes straight into a JSON body, and encoding/json
// replaces an invalid byte sequence with U+FFFD -- so the final token of every
// truncated document was corrupted before it was embedded, and the vector that
// came back described text the document does not contain.

func TestEmbedRequestBodyIsValidUTF8(t *testing.T) {
	var captured string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		var payload struct {
			Input string `json:"input"`
		}
		_ = json.Unmarshal(raw, &payload)
		captured = payload.Input
		_, _ = io.WriteString(w, `{"data":[{"embedding":[0.1,0.2]}]}`)
	}))
	defer server.Close()

	// One leading ASCII byte then three-byte runes: the offset matters,
	// because 8000 is divisible by 2 and 4 and a uniform multi-byte string
	// would happen to cut on a boundary and prove nothing.
	text := "p" + strings.Repeat("界", 4000)

	if _, err := embedOpenAICompatible(context.Background(), text, openAIEmbedCfg{
		BaseURL: server.URL, Model: "test-embed", APIKey: "test",
	}); err != nil {
		t.Fatalf("embed: %v", err)
	}

	if captured == "" {
		t.Fatal("the server captured no input, so this guard checked nothing")
	}
	if !utf8.ValidString(captured) {
		t.Fatal("the embed request carried invalid UTF-8")
	}
	if strings.ContainsRune(captured, '�') {
		t.Fatal("the embed request carried a replacement character: the input " +
			"was cut mid-rune, so the final token describes text the document " +
			"does not contain")
	}
	if len(captured) > 8000 {
		t.Fatalf("the bound moved: %d bytes sent, limit is 8000", len(captured))
	}
}

// TestEmbedRequestIsUnchangedForAsciiInput pins that the fix changed where the
// cut lands, not how much is sent.
func TestEmbedRequestIsUnchangedForAsciiInput(t *testing.T) {
	var captured string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		var payload struct {
			Input string `json:"input"`
		}
		_ = json.Unmarshal(raw, &payload)
		captured = payload.Input
		_, _ = io.WriteString(w, `{"data":[{"embedding":[0.1]}]}`)
	}))
	defer server.Close()

	if _, err := embedOpenAICompatible(context.Background(), strings.Repeat("a", 9000),
		openAIEmbedCfg{BaseURL: server.URL, Model: "test-embed", APIKey: "test"}); err != nil {
		t.Fatal(err)
	}
	if len(captured) != 8000 {
		t.Fatalf("ASCII input was bounded to %d bytes, want 8000", len(captured))
	}
}
