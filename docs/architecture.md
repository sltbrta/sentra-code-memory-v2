# Architecture

Sentra Code Memory is a standalone Go workspace containing the code-related
backend extracted from Ouroboros. The repository owns its model clients,
retrieval adapters, durable projections, generated contracts, and worker
boundaries. No package imports or executes the previous repository.

## System shape

```text
coding agent
    │ direct CLI or JSONL stdin/stdout
    ▼
services/brain/cmd/sentra-code-memory
    │ request validation, aliases, bounded context
    ▼
services/brain/internal/codeserve
    ├── codecrawl      working-tree crawl, cache, search, impact, watch
    ├── productsearch  profile facade and exact P5 symbol search
    └── hosted         local/hosted retrieval, model, embedding, ranker clients

committed source ──► codeindex P5 projections ──► exact defs/refs/imports
working tree ──────► codecrawl code-index.gob ──► heuristic context/search
memory directory ──► memory + hosted/local store ──► optional memory-ask
```

## Module boundaries

- **`cmd/sentra-code-memory`** is the only agent-facing process. It is a thin
  transport adapter: direct subcommands become the same request maps accepted
  by `codeserve`, so direct and JSONL behavior cannot drift.
- **`codeserve`** is the stable protocol boundary. It returns structured JSON
  for every request and keeps unknown verbs, malformed input, and missing
  parameters observable without panics.
- **`codecrawl`** is the fast working-tree lane. It uses multiple workers,
  file stamps before hashing, content-addressed durable state, atomic cache
  publication, and fsnotify/poll watching. Watch events are debounced and
  coalesced in a bounded path queue; refresh failures retry exponentially and
  queue overflow forces an authoritative full stamp/hash reconciliation. Warm
  refreshes avoid body reads when stamps match. Crawls load one repository
  ignore policy from `.gitignore`, `.dockerignore`, `.git/info/exclude`, and
  conservative generated/secret defaults, so ignored files never enter the
  index or watcher refreshes.
- **`codeindex`** is the exact P5 lane. It performs bounded deterministic
  projections for Go, TypeScript, Python, Rust, and Java. Syntax-aware results
  are distinguished from lexical degradation and carry content/receipt digests.
- **`productsearch`** chooses the code profile. Exact search projects files
  individually under the codeindex hard caps, avoiding a whole-repository
  snapshot result overflow on large repositories while retaining deterministic
  receipt coverage and source coordinates. Heuristic lexical ranking applies
  inverse document frequency and multi-token coverage before path/symbol
  boosts, reducing irrelevant single-term and generated-file hits.
- **`memory`** is the projection-only cortex: claims, temporal relations,
  episodes, utility, PageIndex, PPR/PageRank, RAPTOR/community, agent-memory
  tiers, and durable local state. Heavy enrichment remains off the ingest/query
  hot path.
- **`hosted`** owns local and hosted LLM, embedding, reranking, dense, and
  retrieval clients. The offline MLX profile defaults to Liquid
  `LFM2.5-VL-1.6B-8bit`, falls back to Gemma 4 `e2b-it-4bit`, and uses the
  multimodal Qwen3-VL 2B embedding/reranker checkpoints when their local
  endpoints are available. Credentials and endpoints are configuration inputs;
  local code indexing never uploads working-tree source.
- **`workers/code-index`** is an isolated Rust worker boundary for deterministic
  receipts and cross-runtime hardening. It is separate from the Go code-index
  projection and can be built/tested independently.

## Request flow

1. An agent invokes a direct command or sends one JSON object to `serve`.
2. The CLI parses only bounded flags/lines and creates a request with a stable
   snake-case verb.
3. `codeserve.Handle` validates required fields and loads or refreshes the
   requested durable index.
4. The selected lane returns ranked evidence, exact occurrences, or a memory
   answer. A warm code query uses an existing gob and skips refresh when
   `no_refresh` is set.
5. One JSON object is emitted per request. Errors use `ok:false`; malformed
   JSON exits non-zero and does not echo request data.

## Performance and hardening

- Crawl concurrency is controlled by `--workers`; benchmark on the target
  machine rather than assuming more workers are always faster.
- Watch `--debounce`, `--queue-size`, `--retry-initial`, and `--retry-max`
  control edit bursts and failure recovery without losing freshness. With no
  `--root`, the caller's current repository is watched.
- Project caches live under each project's `.sentra` directory, allowing
  multiple agents to watch separate projects concurrently. The optional MLX
  process uses one shared user-local model service, while source/index state
  remains project-scoped.
- Stamp/hash delta refresh prevents unchanged file body reads.
- Durable indexes are written atomically and include the observed Git head.
- Exact projection stays within per-file token/result/coordinate hard caps and
  uses a larger-but-still-hard-bounded profile for large real repositories.
- Exact results include `path:line:column` source locations and a deterministic
  receipt digest.
- Local model/retrieval work is opt-in; no credentials are required for code
  crawl, search, or exact indexing.
- All protocol responses are machine-readable and every operation reports a
  backend and timing field where available.

## Deliberate non-goals

This repository does not include the Ouroboros macOS app, TUI, media/evaluation
worker, deployment infrastructure, or company-only UI. Those surfaces are not
needed by coding agents using the standalone code-memory control plane.
