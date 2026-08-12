# Canonical local HTTP and MCP-stdio adapters

`internal/adapters` exposes the canonical `codeserve` contract over local HTTP
and MCP stdio (Phase 5, issue #35). Both transports reuse `codeserve.Handle` so
the same request map produces the same response map regardless of surface; the
existing JSONL and direct CLI behavior is unchanged.

No new dependencies: the package uses only the Go standard library plus
`codeserve`. Requests are bounded (`MaxRequestBytes = 8 MiB`, matching the CLI
serve limit) and errors are structured (`codeserve.ErrorResponse`).

## Trust policy (issue #41)

The local trust boundary is explicit:

- **Loopback default.** The CLI binds `127.0.0.1:8765` unless told otherwise.
  `ValidateListenAddr` runs before the socket opens and refuses a
  non-loopback bind (`0.0.0.0`, wildcard, LAN) unless a bearer token is set
  or `AllowInsecure` / `--allow-insecure` opts out. Loopback hosts are
  `127.0.0.0/8`, `::1`, `localhost`; an empty host is a wildcard.
- **Bearer token.** Set `HTTPConfig.Token` (CLI `--token`, env
  `SENTRA_CODE_MEMORY_HTTP_TOKEN`) to require
  `Authorization: Bearer <token>` on **every** endpoint including `/health`.
  Failures return HTTP 401 + `WWW-Authenticate: Bearer` with a structured
  codeserve envelope (`error_code: "unauthorized"`).
- **MCP stdio.** The trust boundary is the spawning parent process, which
  owns the child's stdin/stdout; there is no network surface, so no token
  machinery applies. This is deliberate and documented.

See `docs/decisions/0025-local-trust-and-durable-index.md` for the migration
notes and trust-boundary rationale.

## HTTP

`GET /health` → liveness + contract + verbs.
`POST /dispatch` → one `codeserve` request map → one response map.

```go
handler := adapters.NewHTTP(adapters.HTTPConfig{
    Addr: "127.0.0.1:8765", Timeout: 30 * time.Second, Token: os.Getenv("SENTRA_CODE_MEMORY_HTTP_TOKEN"),
})
```

CLI: `sentra-code-memory http --addr 127.0.0.1:8765 [--token …] [--allow-insecure]`

## MCP stdio

A minimal, real MCP server over JSON-RPC 2.0 (newline-delimited on stdio).
Trust: stdio has no network surface — the spawning parent owns the pipes, so
the adapter authenticates nothing and authorizes nothing at this layer (the
`code_read` path gates in codeserve still apply).

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
