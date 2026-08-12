# Canonical local HTTP and MCP-stdio adapters

`internal/adapters` exposes the canonical `codeserve` contract over local HTTP
and MCP stdio (Phase 5, issue #35). Both transports reuse `codeserve.Handle` so
the same request map produces the same response map regardless of surface; the
existing JSONL and direct CLI behavior is unchanged.

No new dependencies: the package uses only the Go standard library plus
`codeserve`. Requests are bounded (`MaxRequestBytes = 8 MiB`, matching the CLI
serve limit) and errors are structured (`codeserve.ErrorResponse`).

## HTTP

`GET /health` → liveness + contract + verbs.
`POST /dispatch` → one `codeserve` request map → one response map.

```go
handler := adapters.NewHTTP(adapters.HTTPConfig{
    Addr: "127.0.0.1:8765", Timeout: 30 * time.Second,
})
```

CLI: `sentra-code-memory http --addr 127.0.0.1:8765`

## MCP stdio

A minimal, real MCP server over JSON-RPC 2.0 (newline-delimited on stdio):

- `initialize` → protocol version, tools capability, server info.
- `notifications/initialized` → completes the handshake (no response).
- `ping` → liveness.
- `tools/list` → every stable `codeserve` verb as a tool.
- `tools/call` → dispatches a verb via `codeserve.Handle`; the response is
  returned as MCP text content. A `codeserve`-level error (`ok:false`) is an
  MCP result with `isError:true` so the agent sees the structured `error_code`
  rather than a transport-level JSON-RPC failure.

`tools/call` accepts `{name: <verb>, arguments: {…}}`; the `verb` field is set
to the tool name unless the arguments already supply one.

```go
err := adapters.ServeMCP(ctx, os.Stdin, os.Stdout, os.Stderr)
```

CLI: `sentra-code-memory mcp`

## Equivalence

CLI, JSONL (`serve`), HTTP (`/dispatch`), and MCP (`tools/call`) normalize to
the same response for the same request. The package test
`TestEquivalencePingCatalog` proves byte-for-byte equality across the three
programmatic surfaces for `ping` and `catalog`.
