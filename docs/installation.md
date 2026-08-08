# Installation and operation

## Prerequisites

- Go 1.26 or newer for the backend workspace.
- Rust 1.95 for the optional `workers/code-index` boundary.
- Git for repository-head and working-tree metadata.

No LLM, embedding service, database, or cloud credential is required for local
code indexing/search.

## Build from source

From the repository root:

```sh
git clone https://github.com/sltbrta/sentra-code-memory-v2.git
cd sentra-code-memory-v2
go build -o ./bin/sentra-code-memory ./services/brain/cmd/sentra-code-memory
./bin/sentra-code-memory catalog
```

The Go workspace has independent `services` and `packages/contracts` modules.
Use the focused local contract before publishing a binary:

```sh
just check
cargo test --locked --offline --manifest-path workers/code-index/Cargo.toml
```

## Agent usage

Index a repository with a durable cache:

```sh
./bin/sentra-code-memory index \
  --root /path/to/repository \
  --index-cache /path/to/cache \
  --workers 8
```

Search the warm cache:

```sh
./bin/sentra-code-memory search \
  --root /path/to/repository \
  --index-cache /path/to/cache \
  --q "extension host startup" \
  --top-k 20 --no-refresh
```

Find exact syntax-aware symbols:

```sh
./bin/sentra-code-memory exact \
  --root /path/to/repository \
  --q IExtensionHostStarter \
  --kind definition --top-k 10
```

Keep the current working repository fresh after edits with the debounced,
retrying watcher. `--root` defaults to the caller's current directory, so an
agent can simply start it from the project it is editing:

```sh
cd /path/to/repository
./bin/sentra-code-memory watch \
  --workers 8 \
  --debounce 300ms \
  --queue-size 4096 \
  --retry-initial 100ms --retry-max 5s
```

The watcher emits JSON refresh/error events. It coalesces file events, retries
failed refreshes, and performs a full authoritative reconciliation if the event
queue overflows. Each project defaults to its own `.sentra/code-index.gob`, so
multiple agents can watch different projects concurrently without sharing
index state. The MLX server/PID/log directory is shared deliberately as a
single local model service; separate projects share model memory but not code
caches. Use `--fsnotify=false` for polling-only environments.

For a persistent process, use JSONL. Each line is independent and produces one
response line:

```sh
printf '%s\n' \
  '{"verb":"code_search","root":"/path/to/repository",'\
  '"q":"extension host","top_k":10}' \
  '{"verb":"code_defs","root":"/path/to/repository",'\
  '"q":"IExtensionHostStarter","top_k":5}' \
  | ./bin/sentra-code-memory serve
```

Use `catalog` for the complete protocol list. Direct aliases (`index`,
`search`, `relevant`, `exact`, `defs`, `refs`, `expand`, `impact`, `route`,
`freshness`, `ingest`, and `memory-ask`) map to those same protocol verbs.

## Optional offline MLX models

On Apple Silicon, `scripts/mlx-serve.sh` launches the OpenAI-compatible local
chat/VLM server. It uses Liquid's latest MLX vision-language checkpoint as the
primary and automatically retries startup with Gemma 4 E2B IT if the primary
cannot load:

- Chat/VLM: `mlx-community/LFM2.5-VL-1.6B-8bit`
- Fallback chat/VLM: `mlx-community/gemma-4-e2b-it-4bit`
- Multimodal embedding: `mlx-community/Qwen3-VL-Embedding-2B-4bit`
- Multimodal reranking: `mlx-community/Qwen3-VL-Reranker-2B-4bit`

Install the local runtimes once (Apple Silicon recommended):

```sh
python3 -m pip install -r scripts/requirements-mlx.txt
```

Start and export the fully local profile:

```sh
./scripts/mlx-serve.sh start
eval "$(./scripts/mlx-serve.sh env)"
./bin/sentra-code-memory mlx status
```

The model repositories are downloaded on first start and then served from the
local Hugging Face cache; subsequent inference is offline. Embedding and
reranking endpoints depend on the installed MLX-compatible server exposing
`/v1/embeddings` and `/v1/rerank`; if unavailable, Sentra retains its bounded
bag/lexical fallbacks rather than making a network request. See
[MLX-LOCAL-INFERENCE.md](runbooks/MLX-LOCAL-INFERENCE.md) for model overrides
and lifecycle recovery.

## Optional model and memory clients

The copied `services/brain/internal/hosted` package owns local/hosted answer,
embedding, reranking, dense, and retrieval clients. Configure those clients
only when using memory/retrieval operations; local code operations do not make
network calls. Keep credentials in the process environment or an untracked
local configuration file. Never commit secrets.

The memory cortex uses a local brain directory for durable projections. The
`memory-ask` command requires both `--dir` and `--q`; it is intentionally
separate from code search so an agent can choose evidence mode explicitly.

## Cache and safety

- Put `--index-cache` outside the source tree when possible.
- Use `--no-refresh` only when the cache freshness is known; otherwise a normal
  search performs a bounded incremental refresh.
- `--force` deliberately rebuilds the code index.
- The crawler and exact search honor root `.gitignore`, `.dockerignore`, and
  `.git/info/exclude`, then apply conservative defaults for secrets, editor
  metadata, dependency trees, caches, generated/build outputs, logs, maps, and
  bytecode. Useful dot-config and `.github` source remains eligible unless a
  repository rule excludes it.
- Indexing/search does not upload working-tree files.
- A malformed JSONL request exits non-zero without returning the input payload.
