package adapters_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/adapters"
	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/codeserve"
)

func TestSCIPAndRecallReachHTTPAndMCP(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "source.go"), []byte("package source\nfunc Anchor() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	document := map[string]any{
		"occurrences": []any{map[string]any{
			"range":  []any{float64(1), float64(5), float64(1), float64(11)},
			"symbol": "scheme pkg m Anchor.", "symbolRoles": float64(1),
		}},
	}
	h := adapters.NewHTTP(adapters.HTTPConfig{})

	for _, tc := range []struct {
		verb string
		args map[string]any
	}{
		{"code_ingest_scip", map[string]any{"root": root, "path": "source.go", "language": "go", "document": document}},
		{"session_recall", map[string]any{"root": root, "q": "Alpha"}},
	} {
		direct := codeserve.Handle(context.Background(), mapToRequest(t, tc.verb, tc.args))
		body, _ := json.Marshal(mapToRequest(t, tc.verb, tc.args))
		httpResp, status := postDispatch(t, h, string(body))
		if status != http.StatusOK {
			t.Fatalf("%s HTTP status=%d", tc.verb, status)
		}
		params, _ := json.Marshal(map[string]any{"name": tc.verb, "arguments": tc.args})
		line, _ := json.Marshal(map[string]any{
			"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": json.RawMessage(params),
		})
		mcpResp := mustToolCallInner(t, serveMCP(t, string(line))[0])

		if !bytes.Equal(normalizeJSON(t, direct), normalizeJSON(t, httpResp)) {
			t.Fatalf("%s direct != HTTP\n%v\n%v", tc.verb, direct, httpResp)
		}
		if !bytes.Equal(normalizeJSON(t, direct), normalizeJSON(t, mcpResp)) {
			t.Fatalf("%s direct != MCP\n%v\n%v", tc.verb, direct, mcpResp)
		}
	}

	tools := adapters.MCPTools()
	found := map[string]bool{}
	for _, tool := range tools {
		found[tool.Name] = true
	}
	for _, verb := range []string{"code_ingest_scip", "session_recall"} {
		if !found[verb] {
			t.Fatalf("MCP tools omit %s", verb)
		}
	}
}
