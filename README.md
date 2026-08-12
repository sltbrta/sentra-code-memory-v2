# Sentra Code Memory

Standalone, low-latency code memory and code intelligence for coding agents.

This repository contains the extracted Go backend and generated contracts from
Ouroboros' code-related system, plus an agent-facing CLI. It is independently
buildable and does **not** import, execute, or require the Ouroboros repository.
The copied backend includes its own local/hosted LLM, embedding, reranking, and
retrieval clients; credentials are optional and supplied through environment or
local configuration.

## Install and use

See [installation.md](docs/installation.md) for prerequisites, build steps,
JSONL usage, cache safety, and optional model clients. See
[architecture.md](docs/architecture.md) for module boundaries and data flow.

```sh
go build -o sentra-code-memory ./services/brain/cmd/sentra-code-memory
./sentra-code-memory catalog
./sentra-code-memory index --root /path/to/repo --workers 8
./sentra-code-memory search --root /path/to/repo --q "authentication" --top-k 20
./sentra-code-memory exact --root /path/to/repo --q "ValidateToken" --kind any
cd /path/to/repo && ./sentra-code-memory watch --workers 8  # watches cwd by default
```

Coding agents can keep one process warm with JSONL:

```sh
printf '%s\n' \
  '{"verb":"code_search","root":"/path/to/repo",'\
  '"q":"authentication","top_k":20,"no_refresh":true}' \
  | ./sentra-code-memory serve
```

The same contract is also exposed over local HTTP and MCP stdio (issue #35):

```sh
./sentra-code-memory http --addr 127.0.0.1:8765   # GET /health, POST /dispatch
printf '%s\n' '{"jsonrpc":"2.0","id":1,"method":"tools/call",'\
  '"params":{"name":"ping","arguments":{}}}' \
  | ./sentra-code-memory mcp
```

Run `catalog` to discover every supported verb. Each input line produces one
JSON response; errors are structured as `ok:false`. A real VS Code benchmark
and answer-quality report is in
[docs/benchmarks/vscode-qa.md](docs/benchmarks/vscode-qa.md).

## Offline model profile

On Apple Silicon, `scripts/mlx-serve.sh` runs a local OpenAI-compatible MLX
server using Liquid `LFM2.5-VL-1.6B-8bit` with Gemma 4 `e2b-it-4bit` fallback.
The configured multimodal retrieval pair is Qwen3-VL Embedding 2B 4-bit plus
Qwen3-VL Reranker 2B 4-bit. Install runtimes with
`scripts/requirements-mlx.txt`; see [docs/installation.md](docs/installation.md)
and
[docs/runbooks/MLX-LOCAL-INFERENCE.md](docs/runbooks/MLX-LOCAL-INFERENCE.md).

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
- Debounced fsnotify/poll watch with coalesced bounded event queue, overflow
  full-rescan protection, exponential retries, and multi-worker refresh.

## Checks

```sh
go test ./services/brain/cmd/sentra-code-memory \
  ./services/brain/internal/codecrawl \
  ./services/brain/internal/codeindex \
  ./services/brain/internal/codeserve \
  ./services/brain/internal/adapters \
  ./services/brain/internal/sessionlog \
  ./services/brain/internal/workflow \
  ./services/brain/internal/memory \
  ./services/brain/internal/productsearch

go vet ./services/brain/cmd/sentra-code-memory \
  ./services/brain/internal/codecrawl \
  ./services/brain/internal/codeindex \
  ./services/brain/internal/codeserve \
  ./services/brain/internal/adapters \
  ./services/brain/internal/sessionlog \
  ./services/brain/internal/workflow \
  ./services/brain/internal/memory \
  ./services/brain/internal/productsearch
```

The full extracted backend also retains authority/evaluation tests that require
optional source-tree evidence or a pinned Bun toolchain; those are intentionally
outside the local coding-agent CLI preflight. See
[docs/standalone.md](docs/standalone.md)
for the control-plane contract and
[docs/specs/code-intelligence/README.md](docs/specs/code-intelligence/README.md)
for the code-intelligence design.
