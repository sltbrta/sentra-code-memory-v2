# Sentra Code Memory standalone contract

This repository is a self-contained extraction of the committed Ouroboros
code-memory and code-intelligence backend. It has its own Go modules, generated
contracts, local/hosted model clients, durable index formats, worker pools, and
agent-facing CLI. It does not import or require the Ouroboros repository.

## Fast path for coding agents

Build once, then use the single binary:

```sh
go build -o sentra-code-memory ./services/brain/cmd/sentra-code-memory
sentra-code-memory catalog
sentra-code-memory index --root /path/to/repo --workers 8
sentra-code-memory search --root /path/to/repo --q "authentication" --top-k 20
sentra-code-memory exact --root /path/to/repo --q "ValidateToken" --kind any
```

For one persistent process, send one JSON object per line to `serve`:

```sh
printf '%s\n' '{"verb":"code_search","root":"/path/to/repo","q":"authentication","top_k":20}' \
  | sentra-code-memory serve
```

Responses are one JSON object per input line. `catalog` is the discovery
contract. `ping` is a local health check. Errors are structured with `ok:false`
and never require a network call.

## Code capabilities

The code surface preserves the multi-worker incremental crawler, stamp/hash warm
refresh, durable `code-index.gob` cache, search/relevant/expand/impact/route/
freshness/ingest paths, and exact syntax definitions/references/imports. Use
`--no-refresh` for the lowest-latency warm read when the cache is known fresh.

The memory surface retains the projection-only cortex, claims, temporal
relations, episodes, utility, PageIndex, PPR/PageRank, RAPTOR/community,
agent-memory tiers, local/hosted retrieval, and bounded background gardener
workers from the extracted backend. Model clients are owned by this repository;
credentials are read only from environment/configuration and never committed.

## Safety and operating boundaries

Indexing and search are local filesystem operations. The CLI does not upload
working-tree source. Hosted retrieval/model calls are opt-in through the copied
client configuration. Paths must be explicitly supplied, cache writes are
atomic/durable, and malformed JSONL requests return a bounded error response.
Run `go test ./...` before publishing a built binary.
