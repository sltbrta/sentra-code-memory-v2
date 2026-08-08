# Sentra Code Memory

Standalone, low-latency code memory and code intelligence for coding agents.

This repository contains the extracted Go backend and generated contracts from
Ouroboros' code-related system, plus an agent-facing CLI. It is independently
buildable and does **not** import, execute, or require the Ouroboros repository.
The copied backend includes its own local/hosted LLM, embedding, reranking, and
retrieval clients; credentials are optional and supplied through environment or
local configuration.

## Install and use

```sh
go build -o sentra-code-memory ./services/brain/cmd/sentra-code-memory
./sentra-code-memory catalog
./sentra-code-memory index --root /path/to/repo --workers 8
./sentra-code-memory search --root /path/to/repo --q "authentication" --top-k 20
./sentra-code-memory exact --root /path/to/repo --q "ValidateToken" --kind any
```

Coding agents can keep one process warm with JSONL:

```sh
printf '%s\n' '{"verb":"code_search","root":"/path/to/repo","q":"authentication","top_k":20,"no_refresh":true}' \
  | ./sentra-code-memory serve
```

Run `catalog` to discover every supported verb. Each input line produces one
JSON response; errors are structured as `ok:false`.

## What is included

- Multi-worker working-tree crawl with stamp/hash incremental refresh.
- Durable atomic `code-index.gob` cache and zero-work warm reads.
- Heuristic search, relevant context, bounded expansion/impact/route/freshness,
  path ingestion, and exact definitions/references/imports.
- Projection-only memory cortex: claims, temporal relations, episodes, utility,
  PageIndex, PPR/PageRank, RAPTOR/community, agent-memory tiers, and gardener.
- Local and hosted retrieval/model clients, dense backends, and generated Go/TS
  contracts.
- Rust code-index worker boundary and parity fixtures.

## Checks

```sh
go test ./services/brain/cmd/sentra-code-memory \
  ./services/brain/internal/codecrawl \
  ./services/brain/internal/codeindex \
  ./services/brain/internal/codeserve \
  ./services/brain/internal/memory

go vet ./services/brain/cmd/sentra-code-memory \
  ./services/brain/internal/codecrawl \
  ./services/brain/internal/codeindex \
  ./services/brain/internal/codeserve \
  ./services/brain/internal/memory
```

The full extracted backend also retains authority/evaluation tests that require
optional source-tree evidence or a pinned Bun toolchain; those are intentionally
outside the local coding-agent CLI preflight. See [docs/standalone.md](docs/standalone.md)
for the control-plane contract and [docs/specs/code-intelligence/README.md](docs/specs/code-intelligence/README.md)
for the code-intelligence design.
