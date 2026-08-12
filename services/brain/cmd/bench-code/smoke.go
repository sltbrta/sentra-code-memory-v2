package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"

	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/adapters"
	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/codeserve"
)

// SmokeProbe is one verb exercised across every client surface.
type SmokeProbe struct {
	Verb string
	Args map[string]any
}

// SmokeResult records whether each surface reached the same outcome.
type SmokeResult struct {
	Verb  string `json:"verb"`
	OK    bool   `json:"ok"`
	CLI   bool   `json:"cli"`
	HTTP  bool   `json:"http"`
	MCP   bool   `json:"mcp"`
	Match bool   `json:"match"`
}

// SmokeMatrix is the multi-client CLI/HTTP/MCP equivalence gate (issue #48).
type SmokeMatrix struct {
	Results []SmokeResult `json:"results"`
	Pass    bool          `json:"pass"`
}

// runSmokeMatrix exercises each probe through the JSONL (direct codeserve),
// HTTP (/dispatch), and MCP (tools/call) surfaces and requires all three to
// agree on the ok flag and verb. It runs fully offline against the fixture.
func runSmokeMatrix(ctx context.Context, root, cache string) (SmokeMatrix, error) {
	probes := []SmokeProbe{
		{Verb: "ping"},
		{Verb: "catalog"},
		{Verb: "code_index", Args: map[string]any{"root": root, "index_cache": cache}},
		{Verb: "code_search", Args: map[string]any{
			"root": root, "index_cache": cache, "q": "ValidateToken", "no_refresh": true,
		}},
		{Verb: "savings_summary", Args: map[string]any{"dir": cache}},
		// A deferred verb must disclose identically on every surface.
		{Verb: "session_product"},
	}

	httpHandler := adapters.NewHTTP(adapters.HTTPConfig{})

	matrix := SmokeMatrix{Pass: true}
	for _, probe := range probes {
		req := codeserve.Request{"verb": probe.Verb}
		for k, v := range probe.Args {
			req[k] = v
		}

		cliResp := codeserve.Handle(ctx, req)
		httpResp, err := dispatchHTTP(httpHandler, req)
		if err != nil {
			return matrix, fmt.Errorf("http %s: %w", probe.Verb, err)
		}
		mcpResp, err := dispatchMCP(ctx, req)
		if err != nil {
			return matrix, fmt.Errorf("mcp %s: %w", probe.Verb, err)
		}

		cliOK, _ := cliResp["ok"].(bool)
		httpOK, _ := httpResp["ok"].(bool)
		mcpOK, _ := mcpResp["ok"].(bool)

		match := cliOK == httpOK && httpOK == mcpOK &&
			sameField(cliResp, httpResp, "verb") && sameField(httpResp, mcpResp, "verb") &&
			sameField(cliResp, httpResp, "error_code") && sameField(httpResp, mcpResp, "error_code")

		res := SmokeResult{Verb: probe.Verb, OK: cliOK, CLI: cliOK, HTTP: httpOK, MCP: mcpOK, Match: match}
		matrix.Results = append(matrix.Results, res)
		if !match {
			matrix.Pass = false
		}
	}
	return matrix, nil
}

func sameField(a, b codeserve.Response, key string) bool {
	av, aok := a[key]
	bv, bok := b[key]
	if aok != bok {
		return false
	}
	if !aok {
		return true
	}
	return fmt.Sprintf("%v", av) == fmt.Sprintf("%v", bv)
}

func dispatchHTTP(h http.Handler, req codeserve.Request) (codeserve.Response, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	httpReq := httptest.NewRequest(http.MethodPost, "/dispatch", bytes.NewReader(body))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httpReq)
	var resp codeserve.Response
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		return nil, err
	}
	return resp, nil
}

func dispatchMCP(ctx context.Context, req codeserve.Request) (codeserve.Response, error) {
	verb, _ := req["verb"].(string)
	args := map[string]any{}
	for k, v := range req {
		if k == "verb" {
			continue
		}
		args[k] = v
	}
	params, err := json.Marshal(map[string]any{"name": verb, "arguments": args})
	if err != nil {
		return nil, err
	}
	line, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": json.RawMessage(params),
	})
	if err != nil {
		return nil, err
	}

	in := strings.NewReader(string(line) + "\n")
	var out, errOut bytes.Buffer
	if err := adapters.ServeMCP(ctx, in, &out, &errOut); err != nil {
		return nil, err
	}

	// Parse the single JSON-RPC response and unwrap the text content.
	var rpc struct {
		Result struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
	}
	first := strings.SplitN(out.String(), "\n", 2)[0]
	if err := json.Unmarshal([]byte(first), &rpc); err != nil {
		return nil, err
	}
	if len(rpc.Result.Content) == 0 {
		return nil, fmt.Errorf("mcp returned no content for %s", verb)
	}
	var resp codeserve.Response
	if err := json.Unmarshal([]byte(rpc.Result.Content[0].Text), &resp); err != nil {
		return nil, err
	}
	return resp, nil
}
