<!-- markdownlint-disable MD013 -->

# MLX local inference lifecycle (BYOC)

**Status:** Packaged script + residual substrate knobs  
**Binary:** `sentra-code-memory` with `OUROBOROS_BRAIN_LLM|EMBED|RANKER=mlx`  
**Script:** [`scripts/mlx-serve.sh`](../../scripts/mlx-serve.sh)  
**Primary chat/VLM:** `mlx-community/LFM2.5-VL-1.6B-8bit`  
**Fallback chat/VLM:** `mlx-community/gemma-4-e2b-it-4bit`

## What this is

Local Apple Silicon (or host) inference that speaks **OpenAI-compatible HTTP**
so
residual company brain can go fully offline:

| Substrate | Endpoint used |
| --- | --- |
| `LLM=mlx` | `POST {base}/chat/completions` |
| `EMBED=mlx` | `POST {base}/embeddings` |
| `RANKER=mlx` | `POST {base}/rerank` (optional; lexical CE if missing) |

Default base: `http://127.0.0.1:8080/v1`

## Lifecycle

```bash
# Start (install mlx-lm first: pip install mlx-lm  OR  uv pip install mlx-lm)
./scripts/mlx-serve.sh start

# Export env for residual
eval "$(./scripts/mlx-serve.sh env)"

# Or via sentra-code-memory helper
sentra-code-memory mlx env
sentra-code-memory mlx status

# Use the standalone agent surface (code paths need no model)
sentra-code-memory index --root /path/to/repo --workers 8
sentra-code-memory search --root /path/to/repo --q "…" --top-k 10
# Optional memory answer, when a local brain directory is populated:
sentra-code-memory memory-ask --dir ~/brains/work --q "…"

# Stop
./scripts/mlx-serve.sh stop
```

## Models

| Env | Default |
| --- | --- |
| `SENTRA_CODE_MEMORY_MLX_CHAT_MODEL` (or legacy `OUROBOROS_BRAIN_MLX_CHAT_MODEL`) | `mlx-community/LFM2.5-VL-1.6B-8bit` |
| `SENTRA_CODE_MEMORY_MLX_CHAT_FALLBACK_MODEL` | `mlx-community/gemma-4-e2b-it-4bit` |
| `SENTRA_CODE_MEMORY_MLX_EMBED_MODEL` | `mlx-community/Qwen3-VL-Embedding-2B-4bit` |
| `SENTRA_CODE_MEMORY_MLX_RANK_MODEL` | `mlx-community/Qwen3-VL-Reranker-2B-4bit` |
| `SENTRA_CODE_MEMORY_MLX_PORT` (or legacy `OUROBOROS_BRAIN_MLX_PORT`) | `8080` |

Override chat model before start:

```bash
SENTRA_CODE_MEMORY_MLX_CHAT_MODEL=mlx-community/gemma-4-e2b-it-4bit ./scripts/mlx-serve.sh start
```

## Fail-closed behavior

If the MLX server is down:

- **embed** → bag-of-words offline dense  
- **llm** → extractive snippets  
- **ranker** → lexical CE  

Ask never hangs forever (HTTP timeouts on MLX client).

## Model provenance and endpoint caveat

The primary/fallback chat choices follow Liquid's MLX model matrix and Google's
Gemma MLX integration. Qwen3-VL embedding and reranking checkpoints provide the
multimodal retrieval pair. Their local runtimes may expose separate embedding
or scoring endpoints rather than the chat server's `/v1/chat/completions`; the
Go client probes the configured endpoint and preserves offline bag/lexical
fallbacks when a specialized endpoint is not running.

- [Liquid LFM model library](https://docs.liquid.ai/lfm/models/complete-library)
- [Liquid LFM2.5-VL-1.6B](https://huggingface.co/LiquidAI/LFM2.5-VL-1.6B)
- [Google Gemma MLX
  integration](https://ai.google.dev/gemma/docs/integrations/mlx)
- [Qwen3-VL
  embedding/reranker](https://huggingface.co/Qwen/Qwen3-VL-Reranker-2B)

## Relation to hosted default

Hosted vendor keys (OpenAI / Cohere / ZE) remain the **default when present**.
MLX is the supported **local** backend only (no Ollama product surface yet).
