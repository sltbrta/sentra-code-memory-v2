package adapters_test

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"

	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/adapters"
	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/codeserve"
)

// TestEquivalenceMemoryOperators proves the new bounded memory operators
// (issue #47) return byte-identical responses across the JSONL, HTTP, and MCP
// surfaces, matching the existing multi-surface equivalence contract. The
// read-only memory_list is used because memory_put mutates the store.
func TestEquivalenceMemoryOperators(t *testing.T) {
	t.Parallel()
	h := adapters.NewHTTP(adapters.HTTPConfig{})
	dir := t.TempDir()

	putArgs := map[string]any{
		"dir": dir, "principal": "alice", "kind": "fact",
		"tier": "stm", "text": "the build uses bazel",
	}

	// Seed one entry through the direct surface. memory_put mutates, so it is
	// not itself equivalence-safe; the read-only memory_list is.
	seed := codeserve.Handle(context.Background(), mapToRequest(t, "memory_put", putArgs))
	if seed["ok"] != true {
		t.Fatalf("seed memory_put: %v", seed)
	}

	listArgs := map[string]any{"dir": dir, "principal": "alice"}

	// Direct JSONL (codeserve.Handle).
	direct := codeserve.Handle(context.Background(), mapToRequest(t, "memory_list", listArgs))
	if direct["ok"] != true {
		t.Fatalf("direct memory_list: %v", direct)
	}

	// HTTP /dispatch.
	body, _ := json.Marshal(mapToRequest(t, "memory_list", listArgs))
	httpResp, code := postDispatch(t, h, string(body))
	if code != 200 {
		t.Fatalf("http memory_list status=%d", code)
	}

	// MCP tools/call.
	params, _ := json.Marshal(map[string]any{"name": "memory_list", "arguments": listArgs})
	reqLine, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": json.RawMessage(params),
	})
	mcpResp := mustToolCallInner(t, serveMCP(t, string(reqLine))[0])

	dj := normalizeJSON(t, direct)
	hj := normalizeJSON(t, httpResp)
	mj := normalizeJSON(t, mcpResp)
	if !bytes.Equal(dj, hj) {
		t.Fatalf("memory_list jsonl != http\n%s\n%s", dj, hj)
	}
	if !bytes.Equal(dj, mj) {
		t.Fatalf("memory_list jsonl != mcp\n%s\n%s", dj, mj)
	}
}

// TestMCPDeferredDisclosureNotCallableTool proves a deferred verb is not
// advertised in tools/list but still yields the structured disclosure if a
// caller invokes it directly over MCP tools/call.
func TestMCPDeferredDisclosureNotCallableTool(t *testing.T) {
	t.Parallel()
	// Not in tools/list.
	r := parseResp(t, serveMCP(t, `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)[0])
	var list struct {
		Tools []map[string]any `json:"tools"`
	}
	if err := json.Unmarshal(r.Result, &list); err != nil {
		t.Fatal(err)
	}
	for _, tool := range list.Tools {
		if tool["name"] == "session_product" {
			t.Fatal("deferred session_product must not be an MCP tool")
		}
	}

	// Invoking it directly returns the deferred disclosure with isError.
	line := `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"session_product","arguments":{}}}`
	resp := parseResp(t, serveMCP(t, line)[0])
	var call struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
		IsError bool `json:"isError"`
	}
	if err := json.Unmarshal(resp.Result, &call); err != nil {
		t.Fatal(err)
	}
	if !call.IsError {
		t.Fatalf("deferred call must set isError: %+v", call)
	}
	var inner codeserve.Response
	if err := json.Unmarshal([]byte(call.Content[0].Text), &inner); err != nil {
		t.Fatal(err)
	}
	if inner["ok"] != false || inner["error_code"] != "deferred" {
		t.Fatalf("deferred disclosure wrong: %+v", inner)
	}
}

// TestHTTPDeferredDisclosure proves the HTTP surface returns the structured
// deferred disclosure with a 200 envelope (ok:false + error_code), matching
// JSONL semantics.
func TestHTTPDeferredDisclosure(t *testing.T) {
	t.Parallel()
	h := adapters.NewHTTP(adapters.HTTPConfig{})
	resp, code := postDispatch(t, h, `{"verb":"hosted_tenancy"}`)
	if code != 200 {
		t.Fatalf("deferred over http want 200 envelope got %d", code)
	}
	if resp["ok"] != false || resp["error_code"] != "deferred" {
		t.Fatalf("http deferred disclosure wrong: %+v", resp)
	}
	if resp["decision"] != "non_goal" {
		t.Fatalf("hosted_tenancy decision=%v want non_goal", resp["decision"])
	}
}

func mapToRequest(t *testing.T, verb string, args map[string]any) codeserve.Request {
	t.Helper()
	req := codeserve.Request{"verb": verb}
	for k, v := range args {
		req[k] = v
	}
	return req
}
