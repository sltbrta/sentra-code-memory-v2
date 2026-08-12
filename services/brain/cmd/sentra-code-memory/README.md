# sentra-code-memory CLI

Small coding-agent control plane over `internal/codeserve`. The repository-wide
installation and architecture contracts live in
[`docs/installation.md`](../../../../docs/installation.md) and
[`docs/architecture.md`](../../../../docs/architecture.md).

```sh
go run ./services/brain/cmd/sentra-code-memory catalog
printf '%s\n' '{"verb":"ping"}' \
  | go run ./services/brain/cmd/sentra-code-memory serve
```

Direct commands are aliases for the JSONL verbs: `index`, `search`, `relevant`,
`exact`, `defs`, `refs`, `read`, `imports`, `expand`, `impact`, `route`,
`freshness`, `ingest`, `repo-map`, `structural`, `diagnostics`,
`apply-changeset`, and `memory-ask`. `apply-changeset` accepts
`--changeset FILE`; the map/search surfaces accept explicit
`--mode fast|quality|deep`, `--max-bytes`, and `--max-tokens`.
`watch` keeps a durable index fresh
with debounce, retries, and multiple workers. It defaults to the caller's
current directory and stores the cache under that project's `.sentra`
directory. This makes simultaneous agents
on separate projects independent. `mlx start|stop|status|env` manages offline
local models.
Every protocol response is JSON. `serve` accepts one JSON object per line and
emits one response per line; malformed input exits non-zero without echoing
request contents.
