<!-- markdownlint-disable MD013 MD060 -->

# Local-first deferred capability audit

- **Status:** answered with an implementation slice
- **As of:** 2026-08-13
- **Informs:** issue #55 and the next local-first implementation stack
- **Decision:** incorporate deterministic/local capabilities first; use Gemini only as an explicit, bounded fallback for capabilities that benefit from language understanding.

## Executive answer

The deferred list is not one bucket. Several capabilities are already implemented
behind package seams and can be safely exposed without a model or network. A
second group can remain local-first with an opt-in Gemini adapter. Hosted tenancy,
billing, cloud overlays, and the full SCM runtime remain outside the standalone
product.

## Capability matrix

| Capability | Classification | Smallest incorporation | Default | Gemini role |
| --- | --- | --- | --- | --- |
| Hybrid ranked retrieval | Local-only | Route `code_find_relevant` through `codecrawl.FindRelevantRanked`; retain deterministic MMR fallback | On | None required |

| Local dense code retrieval | Local-only | Use existing pure-Go dense/HNSW and bag-of-words fallback behind a bounded optional arm | Off until explicitly requested | None required |

| SCIP authority | Local-only | Add bounded `code_ingest_scip` accepting user-generated SCIP JSON and persist authority-tagged edges | Explicit request only | None |
| Affected test hints / related context | Local-only | Expose existing `ImpactReceipt.AffectedTests` and compose existing expand/impact/repo-map surfaces | On in existing receipts | None |
| Session recall | Local-only | Add bounded `session_recall` over repo-local `sessionlog.Recall`, with abstention | Explicit request only | None |
| Lifecycle hooks | Local-only | Add reversible `hooks install/status/uninstall` under `.sentra` or `.git/hooks` | Explicit request only | None |
| Query expansion / patch-plan hints | Local + optional LLM | Add a small adapter interface; use deterministic query tokens first and Gemini only with explicit opt-in | Off | Optional Gemini 3.6 Flash |
| Semantic reranking | Local + optional LLM | Preserve lexical/ranked output; optionally score a bounded candidate set through an adapter | Off | Optional, bounded Gemini scoring; deterministic fallback |
| Claim/OpenIE extraction | Local + optional LLM | Reuse `memory.LLMExtractFunc` with strict JSON parsing, provenance, and abstention | Off | Optional Gemini extraction |
| Structural rewrite | Local-first but safety-sensitive | Keep deterministic search/preview; require explicit ChangeSet and verification for writes | Preview only | Optional planning only; never direct writes |
| Tree-sitter broad syntax | Local-only in principle | Add only when dependency/build budget is accepted; SCIP remains explicit authority input | Deferred | None |
| Full SCM session product | Not standalone-owned | Keep bounded continuation/recall only | Deferred | Not a substitute for product scope |
| Lifecycle login/hosted sync/tenancy/billing | Inherently hosted or product-bound | Keep deferred/non-goal | Deferred | Not applicable |

## Gemini adapter contract

Google documents the stable model identifier as `gemini-3.6-flash`. Integration
uses the official Google Gemini Go SDK (`google.golang.org/genai`), not
hand-written HTTP and not the OpenAI-compatible endpoint. The adapter
(`services/brain/internal/llmadapter`) is opt-in and is never required for a
successful local request:

- key: `GEMINI_API_KEY` (no key means deterministic local behavior);
- model default: `gemini-3.6-flash`, overrideable for testing/operations via
  `SENTRA_CODE_MEMORY_GEMINI_MODEL`;
- strict per-call timeout clamped to the caller deadline, plus maximum
  input/output bytes/tokens;
- structured JSON responses through the SDK's response-schema support, with
  strict local validation before use;
- redact credentials, absolute workspace paths, and source outside the bounded
  candidate set before transmission;
- no automatic retries that exceed the caller deadline;
- any transport, parse, or policy failure returns the deterministic fallback;
- response diagnostics identify `llm_used`, model, bounded candidate count, and
  fallback reason without including prompt/source content;
- no write-capable tool calls: Gemini may propose ranking/query/plan data, while
  deterministic code validates and applies any ChangeSet.

Gemini is cloud-hosted rather than an offline local model. The local-first
fallback therefore remains mandatory. For fully offline model execution, the
existing MLX/Gemma path remains the local alternative.

## Evidence

- **Current package seams:** `services/brain/internal/codecrawl/verbs.go` exposes
  `FindRelevantRanked`; `services/brain/internal/codecrawl/scip.go` exposes
  `Index.IngestSCIP`; `services/brain/internal/sessionlog/recall.go` exposes
  abstaining `Recall`; `services/brain/internal/memory/extract_llm.go` exposes
  strict injected extraction; `services/brain/internal/rerank/types.go` defines
  `Embedder` and `Reranker` seams.
- **Current local model path:** `services/brain/internal/hosted/api_substrate.go`,
  `scripts/mlx-serve.sh`, and the `SENTRA_CODE_MEMORY_MLX_*` settings.
- **Optional LLM seam:** `services/brain/internal/llmadapter` implements the
  provider-neutral adapter (query expansion, semantic scoring, claim
  extraction) with deterministic local fallback and a Gemini implementation on
  the official Go SDK. `services/brain/internal/memory/extract_llm.go` remains
  the injected claim-extraction seam the adapter can back.
- **Google model details:** [Gemini 3.6 Flash model page](https://ai.google.dev/gemini-api/docs/models/gemini-3.6-flash),
  [Go SDK (google.golang.org/genai)](https://pkg.go.dev/google.golang.org/genai),
  [structured output](https://ai.google.dev/gemini-api/docs/structured-output).
- **Prior parity evidence:** `docs/research/2026-08-12-parity-audit-and-remaining-work.md`,
  `docs/research/2026-07-25-codecrawl-scm-parity.md`, and
  `docs/decisions/0025-memory-session-lifecycle-parity.md`.

## Recommended sequence

1. Expose existing ranked retrieval, SCIP ingestion, test hints, and bounded
   session recall with deterministic tests.
2. Add the provider-neutral optional LLM interface and Gemini implementation with
   redaction, budgets, and fallback tests.
3. Wire Gemini only into query expansion/semantic scoring; do not put it on the
   critical path for indexing, reads, or ChangeSet application.
4. Revisit local dense retrieval and reversible hook management separately after
   measuring the first slice.

## Non-goals

This audit does not authorize hosted tenancy, billing, cloud synchronization,
full latent session memory, direct model-driven filesystem writes, or claims of
compiler-grade authority from heuristic/index-only data.
