package adapters

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/codeserve"
)

// MCP protocol constants. The stdio transport is newline-delimited JSON-RPC
// 2.0; stdout must carry only protocol messages, so diagnostics go to stderr.
const (
	mcpProtocolVersion = "2025-06-18"

	jsonRPCVersion = "2.0"

	// JSON-RPC error codes.
	codeParseError     = -32700
	codeInvalidRequest = -32600
	codeMethodNotFound = -32601
	codeInvalidParams  = -32602
	codeInternalError  = -32603
)

// MCP-level operator opt-in field name. When a verb carries
// VerbSpec.RequiresOperatorTrust (issue #63), its mutating actions
// (install/uninstall/run) are refused at tools/call dispatch unless the
// JSON arguments carry "_operator_trust": true. codeserve will neither
// recognize nor forward this field because codeserve.Handle only reads
// the verb-defined keys; the adapter owns the gating decision so the
// surface stays self-documenting for an MCP client.
const operatorTrustOptInArgKey = "_operator_trust"

// rpcRequest is a JSON-RPC 2.0 request/notification.
type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// rpcResponse is a JSON-RPC 2.0 response.
type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// MCPTool describes one exposed codeserve verb as an MCP tool.
type MCPTool struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	InputSchema map[string]any `json:"inputSchema"`
}

// mcpServerInfo is the initialize result serverInfo field.
type mcpServerInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// mcpCapabilities is the initialize result capabilities field.
type mcpCapabilities struct {
	Tools map[string]any `json:"tools,omitempty"`
}

// mcpInitializeResult is the initialize result.
type mcpInitializeResult struct {
	ProtocolVersion string          `json:"protocolVersion"`
	Capabilities    mcpCapabilities `json:"capabilities"`
	ServerInfo      mcpServerInfo   `json:"serverInfo"`
}

// mcpToolListResult is the tools/list result.
type mcpToolListResult struct {
	Tools []MCPTool `json:"tools"`
}

// mcpTextContent is one text content block in a tools/call result.
type mcpTextContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// mcpCallResult is the tools/call result.
type mcpCallResult struct {
	Content []mcpTextContent `json:"content"`
	IsError bool             `json:"isError,omitempty"`
}

// MCPTools builds the tool catalog from codeserve metadata. Each tool is one
// verb with a permissive JSON Schema built from its required/optional fields.
// Verbs whose catalog spec carries RequiresOperatorTrust are still
// advertised (so tools/list accurately reflects capability), but their
// description and schema explicitly document the explicit operator
// opt-in needed to dispatch the mutating actions (issue #63).
func MCPTools() []MCPTool {
	specs := codeserve.CatalogMetadata()
	tools := make([]MCPTool, 0, len(specs))
	for _, s := range specs {
		if s.Status != codeserve.StatusStable {
			continue // deferred verbs have no handler; do not advertise them
		}
		props := map[string]any{}
		required := []string{}
		for _, f := range s.Required {
			props[f] = mcpFieldSchema(f)
			required = append(required, f)
		}
		for _, f := range s.Optional {
			props[f] = mcpFieldSchema(f)
		}
		if s.RequiresOperatorTrust {
			props[operatorTrustOptInArgKey] = map[string]any{
				"type":        "boolean",
				"description": "explicit operator opt-in (issue #63) required to dispatch mutating actions (install/uninstall/run); status is always admitted.",
			}
		}
		schema := map[string]any{
			"type":                 "object",
			"properties":           props,
			"additionalProperties": true,
		}
		if len(required) > 0 {
			schema["required"] = required
		}
		desc := s.Summary
		if len(s.Aliases) > 0 {
			desc = desc + " (aliases: " + strings.Join(s.Aliases, ", ") + ")"
		}
		if s.RequiresOperatorTrust {
			desc = desc + " Mutating actions (install/uninstall/run) over MCP require the explicit _operator_trust=true opt-in; status and read-only actions are always admitted. Use the direct CLI for one-off installs."
		}
		tools = append(tools, MCPTool{
			Name: s.Name, Description: desc, InputSchema: schema,
		})
	}
	return tools
}

// mcpFieldSchema mirrors the concrete JSON values accepted by codeserve's
// intField and boolField helpers. Unknown fields remain strings by default.
func mcpFieldSchema(field string) map[string]any {
	switch field {
	case "workers", "top_k", "max_bytes", "max_tokens", "start_line",
		"max_lines", "max_depth", "max_files", "max_matches", "max_bridges", "interval_ms",
		"debounce_ms", "queue_size", "retry_initial_ms", "retry_max_ms",
		"max_cycles", "timeout_ms", "l0_bytes", "l1_bytes", "l2_bytes", "l3_bytes":
		return map[string]any{"type": "integer"}
	case "force", "no_refresh", "preview", "fsnotify", "include_superseded",
		"allow_ignored", "allow_unindexed":
		return map[string]any{"type": "boolean"}
	case "changeset", "document":
		return map[string]any{"type": "object"}
	case "min_confidence", "min_relevance":
		return map[string]any{"type": "number", "minimum": 0, "maximum": 1}
	case "mode":
		return map[string]any{"type": "string", "enum": []string{"fast", "quality", "deep"}}
	case "paths":
		return map[string]any{"oneOf": []any{
			map[string]any{"type": "string"},
			map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
		}}
	default:
		return map[string]any{"type": "string"}
	}
}

// ServeMCP runs the MCP stdio loop: read newline-delimited JSON-RPC requests
// from in, write responses to out, and diagnostics to errOut. It returns when
// in reaches EOF. The loop is bounded: lines longer than MaxRequestBytes are
// rejected as parse errors. It reuses codeserve.Handle so behavior matches the
// JSONL/CLI/HTTP surfaces (issue #35 equivalence).
func ServeMCP(ctx context.Context, in io.Reader, out, errOut io.Writer) error {
	reader := bufio.NewReader(in)
	enc := json.NewEncoder(out)
	for {
		line, tooLong, readErr := readMCPLine(reader)
		if tooLong {
			responses := []*rpcResponse{{JSONRPC: jsonRPCVersion, Error: &rpcError{
				Code: codeParseError, Message: "parse error: request line exceeds maximum size",
			}}}
			for _, resp := range responses {
				if err := enc.Encode(resp); err != nil {
					fmt.Fprintf(errOut, "mcp: write response: %v\n", err)
					return err
				}
			}
		}
		if readErr == io.EOF && len(line) == 0 && !tooLong {
			break
		}
		if readErr != nil && readErr != io.EOF {
			return readErr
		}
		if tooLong {
			if readErr == io.EOF {
				break
			}
			continue
		}
		if len(strings.TrimSpace(string(line))) == 0 {
			if readErr == io.EOF {
				break
			}
			continue
		}
		responses := handleMCPLine(ctx, line)
		for _, resp := range responses {
			if resp == nil {
				continue // notification: no response
			}
			if err := enc.Encode(resp); err != nil {
				fmt.Fprintf(errOut, "mcp: write response: %v\n", err)
				return err
			}
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if readErr == io.EOF {
			break
		}
	}
	return nil
}

// readMCPLine consumes exactly one line while retaining at most the protocol
// limit. Oversized lines are drained so the next request can still be served.
func readMCPLine(reader *bufio.Reader) ([]byte, bool, error) {
	var line []byte
	lineBytes := 0
	tooLong := false
	for {
		part, err := reader.ReadSlice('\n')
		if !tooLong {
			partBytes := len(part)
			if len(part) > 0 && part[len(part)-1] == '\n' {
				partBytes--
			}
			if lineBytes+partBytes > MaxRequestBytes {
				tooLong = true
				line = nil
			} else {
				line = append(line, part...)
				lineBytes += partBytes
			}
		}
		if err == bufio.ErrBufferFull {
			continue
		}
		return line, tooLong, err
	}
}

// handleMCPLine parses one JSON-RPC line and returns the responses to emit.
// Notifications (no id) return a single nil entry (emit nothing). Parse errors
// return a single -32700 response with null id per the JSON-RPC spec.
func handleMCPLine(ctx context.Context, line []byte) []*rpcResponse {
	var req rpcRequest
	if err := json.Unmarshal(line, &req); err != nil {
		return []*rpcResponse{{JSONRPC: jsonRPCVersion, Error: &rpcError{
			Code: codeParseError, Message: "parse error: " + err.Error(),
		}}}
	}
	if req.JSONRPC != jsonRPCVersion {
		return []*rpcResponse{{JSONRPC: jsonRPCVersion, ID: req.ID, Error: &rpcError{
			Code: codeInvalidRequest, Message: "jsonrpc must be \"2.0\"",
		}}}
	}
	// Notifications carry no id and receive no response.
	if len(req.ID) == 0 {
		_ = dispatchMCPNotification(req.Method) // best-effort; nothing to emit
		return []*rpcResponse{nil}
	}
	result, rerr := dispatchMCPMethod(ctx, req.Method, req.Params)
	resp := &rpcResponse{JSONRPC: jsonRPCVersion, ID: req.ID}
	if rerr != nil {
		resp.Error = rerr
	} else {
		resp.Result = result
	}
	return []*rpcResponse{resp}
}

// dispatchMCPNotification handles notifications (no response). The initialized
// notification completes the handshake; unknown notifications are ignored.
func dispatchMCPNotification(method string) error {
	switch method {
	case "notifications/initialized":
		return nil
	default:
		return nil
	}
}

// dispatchMCPMethod routes a method to its handler.
func dispatchMCPMethod(ctx context.Context, method string, params json.RawMessage) (any, *rpcError) {
	switch method {
	case "initialize":
		return mcpInitializeResult{
			ProtocolVersion: mcpProtocolVersion,
			Capabilities:    mcpCapabilities{Tools: map[string]any{}},
			ServerInfo:      mcpServerInfo{Name: ServerName, Version: ServerVersion},
		}, nil
	case "ping":
		return map[string]any{}, nil
	case "tools/list":
		return mcpToolListResult{Tools: MCPTools()}, nil
	case "tools/call":
		return dispatchMCPToolCall(ctx, params)
	default:
		return nil, &rpcError{Code: codeMethodNotFound, Message: "method not found: " + method}
	}
}

// toolsCallParams is the tools/call params shape.
type toolsCallParams struct {
	Name      string         `json:"name"`
	Arguments map[string]any `json:"arguments,omitempty"`
}

// dispatchMCPToolCall maps a tool name to a codeserve verb, dispatches it, and
// wraps the response as MCP text content. codeserve-level errors (ok:false) are
// returned as successful MCP results with isError:true so the agent sees the
// structured error_code rather than a transport-level JSON-RPC failure.
//
// Operator-trust gate (issue #63): mutating actions on verbs whose catalog
// spec carries RequiresOperatorTrust (currently hooks_local install /
// uninstall / run) are refused unless the JSON arguments explicitly
// include "_operator_trust": true. The check runs before codeserve
// dispatch and the gate's structured codeserve envelope is forwarded
// verbatim so an MCP client sees the same error_code as HTTP.
func dispatchMCPToolCall(ctx context.Context, params json.RawMessage) (any, *rpcError) {
	var p toolsCallParams
	if len(params) > 0 {
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, &rpcError{Code: codeInvalidParams, Message: "invalid params: " + err.Error()}
		}
	}
	name := strings.TrimSpace(p.Name)
	if name == "" {
		return nil, &rpcError{Code: codeInvalidParams, Message: "tool name required"}
	}
	verb := name
	if v, ok := p.Arguments["verb"].(string); ok && strings.TrimSpace(v) != "" {
		verb = strings.TrimSpace(v)
	}
	if codeserve.IsOperatorTrustAction(verb, mcpActionString(p.Arguments["action"])) {
		if !mcpOperatorTrustOptedIn(p.Arguments) {
			body, _ := json.Marshal(codeserve.OperatorTrustError(verb, mcpActionString(p.Arguments["action"]), "mcp"))
			return mcpCallResult{
				Content: []mcpTextContent{{Type: "text", Text: string(body)}},
				IsError: true,
			}, nil
		}
	}
	req := codeserve.Request{}
	for k, v := range p.Arguments {
		if k == operatorTrustOptInArgKey {
			continue // never forward the opt-in field to codeserve; codeserve would ignore it but staying symmetric with HTTP keeps the contract obvious.
		}
		req[k] = v
	}
	if _, ok := req["verb"]; !ok {
		req["verb"] = name
	}
	resp := dispatch(ctx, req)
	body, _ := json.Marshal(resp)
	isErr := false
	if ok, _ := resp["ok"].(bool); !ok {
		isErr = true
	}
	return mcpCallResult{
		Content: []mcpTextContent{{Type: "text", Text: string(body)}},
		IsError: isErr,
	}, nil
}

// mcpActionString normalizes the action field the same way codeserve.str
// would, so " install " and "install" map to the same trust decision.
func mcpActionString(v any) string {
	s, _ := v.(string)
	return strings.TrimSpace(s)
}

// mcpOperatorTrustOptedIn reports whether the JSON arguments map carries
// "_operator_trust": true. Accepting the bool form keeps it
// self-documenting in tools/list descriptions; other shapes (string "1",
// string "true") are intentionally not honored so the opt-in is explicit
// and hard to inject accidentally through JSON schema coercion.
func mcpOperatorTrustOptedIn(args map[string]any) bool {
	v, ok := args[operatorTrustOptInArgKey]
	if !ok {
		return false
	}
	b, ok := v.(bool)
	return ok && b
}
