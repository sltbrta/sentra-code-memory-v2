package adapters_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/adapters"
	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/codeserve"
)

// normalizeJSON returns canonical JSON for a codeserve response so the three
// surfaces can be compared byte-for-byte regardless of transport framing.
func normalizeJSON(t *testing.T, resp codeserve.Response) []byte {
	t.Helper()
	raw, err := json.Marshal(resp)
	if err != nil {
		t.Fatal(err)
	}
	var canonical map[string]any
	if err := json.Unmarshal(raw, &canonical); err != nil {
		t.Fatal(err)
	}
	out, err := json.Marshal(canonical)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// postDispatch issues a /dispatch request and returns the decoded response.
func postDispatch(t *testing.T, h http.Handler, body string) (codeserve.Response, int) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/dispatch", strings.NewReader(body))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	var resp codeserve.Response
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode dispatch response: %v (body=%s)", err, rr.Body.String())
	}
	return resp, rr.Code
}

// serveMCP runs the MCP stdio loop over the joined input lines and returns the
// non-empty JSON-RPC response lines.
func serveMCP(t *testing.T, lines ...string) []string {
	t.Helper()
	in := strings.NewReader(strings.Join(lines, "\n") + "\n")
	var out, errOut bytes.Buffer
	if err := adapters.ServeMCP(context.Background(), in, &out, &errOut); err != nil {
		t.Fatalf("ServeMCP: %v (stderr=%s)", err, errOut.String())
	}
	var resp []string
	for _, l := range strings.Split(out.String(), "\n") {
		if strings.TrimSpace(l) != "" {
			resp = append(resp, l)
		}
	}
	return resp
}

// rpcResp is the parsed JSON-RPC response used by the MCP assertions.
type rpcResp struct {
	ID     json.RawMessage `json:"id"`
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func parseResp(t *testing.T, line string) rpcResp {
	t.Helper()
	var r rpcResp
	if err := json.Unmarshal([]byte(line), &r); err != nil {
		t.Fatalf("parse rpc response %q: %v", line, err)
	}
	return r
}

func mustToolCallInner(t *testing.T, line string) codeserve.Response {
	t.Helper()
	r := parseResp(t, line)
	var res struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(r.Result, &res); err != nil {
		t.Fatal(err)
	}
	if len(res.Content) != 1 {
		t.Fatalf("tool call missing content: %s", line)
	}
	var inner codeserve.Response
	if err := json.Unmarshal([]byte(res.Content[0].Text), &inner); err != nil {
		t.Fatal(err)
	}
	return inner
}

func TestEquivalencePingCatalog(t *testing.T) {
	t.Parallel()
	h := adapters.NewHTTP(adapters.HTTPConfig{})

	for _, verb := range []string{"ping", "catalog"} {
		direct := codeserve.Handle(context.Background(), codeserve.Request{"verb": verb})

		httpResp, code := postDispatch(t, h, `{"verb":"`+verb+`"}`)
		if code != http.StatusOK {
			t.Fatalf("%s: want 200 got %d", verb, code)
		}

		params, _ := json.Marshal(map[string]any{"name": verb, "arguments": map[string]any{}})
		reqLine, _ := json.Marshal(map[string]any{
			"jsonrpc": "2.0", "id": 1, "method": "tools/call",
			"params": json.RawMessage(params),
		})
		lines := serveMCP(t, string(reqLine))
		if len(lines) != 1 {
			t.Fatalf("%s: want 1 mcp response, got %d", verb, len(lines))
		}
		mcpResp := mustToolCallInner(t, lines[0])

		dj := normalizeJSON(t, direct)
		hj := normalizeJSON(t, httpResp)
		mj := normalizeJSON(t, mcpResp)
		if !bytes.Equal(dj, hj) {
			t.Fatalf("%s: jsonl != http\ndirect=%s\nhttp=%s", verb, dj, hj)
		}
		if !bytes.Equal(dj, mj) {
			t.Fatalf("%s: jsonl != mcp\ndirect=%s\nmcp=%s", verb, dj, mj)
		}
	}
}

func TestHTTPErrorsStructured(t *testing.T) {
	t.Parallel()
	h := adapters.NewHTTP(adapters.HTTPConfig{})

	// Wrong method on dispatch → 405 structured error.
	getRR := httptest.NewRecorder()
	h.ServeHTTP(getRR, httptest.NewRequest(http.MethodGet, "/dispatch", nil))
	if getRR.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET dispatch want 405 got %d", getRR.Code)
	}

	// Invalid JSON → 400 structured codeserve error.
	resp, code := postDispatch(t, h, `{not json}`)
	if code != http.StatusBadRequest {
		t.Fatalf("invalid json want 400 got %d", code)
	}
	if resp["ok"] != false || resp["error_code"] != string(codeserve.ErrInvalidRequest) {
		t.Fatalf("invalid json not structured: %+v", resp)
	}

	// Non-object body → 400.
	resp, code = postDispatch(t, h, `42`)
	if code != http.StatusBadRequest {
		t.Fatalf("non-object want 400 got %d", code)
	}
	if resp["ok"] != false {
		t.Fatalf("non-object not ok:false: %+v", resp)
	}
}

func TestHTTPBoundedRequest(t *testing.T) {
	t.Parallel()
	h := adapters.NewHTTP(adapters.HTTPConfig{})
	huge := `{"verb":"ping","padding":"` + strings.Repeat("x", adapters.MaxRequestBytes) + `"}`
	resp, code := postDispatch(t, h, huge)
	if code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversize want 413 got %d (resp=%+v)", code, resp)
	}
}

func TestHealthEndpoint(t *testing.T) {
	t.Parallel()
	h := adapters.NewHTTP(adapters.HTTPConfig{})
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/health", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("health want 200 got %d", rr.Code)
	}
	var resp codeserve.Response
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp["ok"] != true || resp["contract"] != codeserve.ContractID {
		t.Fatalf("health wrong: %+v", resp)
	}
}

func TestMCPInitializeAndToolsList(t *testing.T) {
	t.Parallel()
	r := parseResp(t, serveMCP(t, `{"jsonrpc":"2.0","id":1,"method":"initialize"}`)[0])
	var res map[string]any
	if err := json.Unmarshal(r.Result, &res); err != nil {
		t.Fatal(err)
	}
	if res["protocolVersion"] == nil {
		t.Fatalf("initialize missing protocolVersion: %+v", res)
	}
	serverInfo, _ := res["serverInfo"].(map[string]any)
	if serverInfo["name"] != adapters.ServerName {
		t.Fatalf("serverInfo.name wrong: %+v", serverInfo)
	}

	r = parseResp(t, serveMCP(t, `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)[0])
	var list struct {
		Tools []map[string]any `json:"tools"`
	}
	if err := json.Unmarshal(r.Result, &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Tools) == 0 {
		t.Fatal("tools/list empty")
	}
	names := map[string]bool{}
	for _, tm := range list.Tools {
		names[tm["name"].(string)] = true
	}
	for _, v := range codeserve.Catalog() {
		if !names[v] {
			t.Fatalf("stable verb %q not advertised by tools/list", v)
		}
	}
}

func TestMCPPingAndUnknownMethod(t *testing.T) {
	t.Parallel()
	r := parseResp(t, serveMCP(t, `{"jsonrpc":"2.0","id":1,"method":"ping"}`)[0])
	var res map[string]any
	if err := json.Unmarshal(r.Result, &res); err != nil {
		t.Fatal(err)
	}
	if len(res) != 0 {
		t.Fatalf("ping result should be empty object: %+v", res)
	}
	r = parseResp(t, serveMCP(t, `{"jsonrpc":"2.0","id":2,"method":"bogus/method"}`)[0])
	if r.Error == nil || r.Error.Code != -32601 {
		t.Fatalf("unknown method want -32601, got %+v", r.Error)
	}
}

func TestMCPParseError(t *testing.T) {
	t.Parallel()
	r := parseResp(t, serveMCP(t, `{not valid json}`)[0])
	if r.Error == nil || r.Error.Code != -32700 {
		t.Fatalf("parse error want -32700, got %+v", r.Error)
	}
}

func TestMCPNotificationNoResponse(t *testing.T) {
	t.Parallel()
	lines := serveMCP(t, `{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	if len(lines) != 0 {
		t.Fatalf("notification must not emit a response, got %+v", lines)
	}
}

func TestMCPStdioLoopRoundTrip(t *testing.T) {
	t.Parallel()
	lines := serveMCP(t,
		`{"jsonrpc":"2.0","id":1,"method":"initialize"}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"ping","arguments":{}}}`,
		"", // trailing blank line tolerated
	)
	// Exactly two responses (initialize + tools/call); the notification emits none.
	if len(lines) != 2 {
		t.Fatalf("want 2 responses, got %d (%q)", len(lines), lines)
	}
	r := parseResp(t, lines[1])
	var id struct {
		ID int `json:"id"`
	}
	_ = json.Unmarshal([]byte(lines[1]), &id)
	if id.ID != 2 {
		t.Fatalf("second response must be id=2, got %s", lines[1])
	}
	inner := mustToolCallInner(t, lines[1])
	if inner["ok"] != true || inner["verb"] != "ping" {
		t.Fatalf("tools/call inner response wrong: %+v", inner)
	}
	_ = r
}
