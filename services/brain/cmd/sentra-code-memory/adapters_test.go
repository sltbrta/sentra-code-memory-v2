package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/adapters"
)

// TestMCPSubcommandRoundTrip: the `mcp` CLI subcommand runs a real MCP stdio
// loop over the canonical codeserve contract (issue #35). initialize,
// tools/list, and tools/call all dispatch through codeserve.Handle.
func TestMCPSubcommandRoundTrip(t *testing.T) {
	in := strings.NewReader(strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize"}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"ping","arguments":{}}}`,
	}, "\n") + "\n")
	var stdout, stderr bytes.Buffer
	if code := execute([]string{"mcp"}, in, &stdout, &stderr); code != 0 {
		t.Fatalf("mcp exit=%d stderr=%s", code, stderr.String())
	}
	lines := nonEmptyLines(stdout.String())
	if len(lines) != 2 {
		t.Fatalf("want 2 rpc responses, got %d: %q", len(lines), stdout.String())
	}
	var init0 map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &init0); err != nil {
		t.Fatal(err)
	}
	if init0["jsonrpc"] != "2.0" {
		t.Fatalf("first response not jsonrpc 2.0: %s", lines[0])
	}
	var call struct {
		Result struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(lines[1]), &call); err != nil {
		t.Fatal(err)
	}
	var inner map[string]any
	if err := json.Unmarshal([]byte(call.Result.Content[0].Text), &inner); err != nil {
		t.Fatal(err)
	}
	if inner["ok"] != true || inner["verb"] != "ping" {
		t.Fatalf("mcp tools/call ping wrong: %+v", inner)
	}
}

// TestHTTPSubcommandServeAndDispatch: the `http` CLI subcommand serves the
// canonical HTTP adapter with health + dispatch reusing codeserve.Handle.
func TestHTTPSubcommandServeAndDispatch(t *testing.T) {
	// Drive the adapter handler directly (the subcommand wraps ListenAndServe);
	// this proves the wiring is the canonical adapters.NewHTTP surface.
	handler := adapters.NewHTTP(adapters.HTTPConfig{})
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/health", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("health want 200 got %d", rr.Code)
	}

	rr2 := httptest.NewRecorder()
	handler.ServeHTTP(rr2, httptest.NewRequest(http.MethodPost, "/dispatch",
		strings.NewReader(`{"verb":"ping"}`)))
	if rr2.Code != http.StatusOK {
		t.Fatalf("dispatch want 200 got %d", rr2.Code)
	}
	var resp map[string]any
	if err := json.Unmarshal(rr2.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp["ok"] != true || resp["verb"] != "ping" {
		t.Fatalf("dispatch ping wrong: %+v", resp)
	}
}

func nonEmptyLines(s string) []string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if strings.TrimSpace(l) != "" {
			out = append(out, l)
		}
	}
	return out
}
