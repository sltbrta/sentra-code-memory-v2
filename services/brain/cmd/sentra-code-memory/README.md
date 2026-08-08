# sentra-code-memory CLI

Small coding-agent control plane over `internal/codeserve`.

```sh
go run ./services/brain/cmd/sentra-code-memory catalog
printf '%s\n' '{"verb":"ping"}' | go run ./services/brain/cmd/sentra-code-memory serve
```

Direct commands are aliases for the JSONL verbs: `index`, `search`, `relevant`,
`exact`, `defs`, `refs`, `expand`, `impact`, `route`, `freshness`, `ingest`, and
`memory-ask`. Every response is JSON. `serve` accepts one JSON object per line
and emits one response per line; malformed input exits non-zero without echoing
request contents.
