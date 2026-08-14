<!-- markdownlint-disable MD060 MD040 -->

# Local Dense Code Retrieval Arm (issue #59)

## Scope and non-goals

This document captures the optional, opt-in local-first dense code retrieval
arm added in response to issue #59. It composes pre-existing pure-Go
primitives already shipped in the standalone product:

- `services/brain/internal/dense.HNSW` — pure-Go hierarchical NSW ANN index
  with atomic Save/Load and identity-bound serving.
- `services/brain/internal/dense.MemoryStore` — in-process cosine bag.
- `services/brain/internal/query.BagOfWordsDense` — sparse bag-of-words
  cosine with deterministic lexical fallback.

The new `denselocal` package adds the policy guard-rails that make
local-first dense retrieval safe to call: model identity binding, bounded
corpora and dimensions, deterministic lexical fallback, and an explicit
no-network contract.

This surface never imports a network package and never changes the
defaults of any existing code operator. The deferred
`code_dense_rerank` catalog verb (ADR 0025) covers the credentialed,
hosted, multi-tenant dense/rerank lane; this surface is the local,
identity-bound, deterministic counterpart that does not require a backend.

## Identification

Every load+search call carries four identity fields:

| Field         | Source of truth                                  |
| ------------- | ------------------------------------------------ |
| `scope`       | the generation/repo the index was built for      |
| `model`       | the embedding model name                         |
| `dimensions`  | the fixed vector dimension                       |
| `content_digest` | a canonical SHA-256 over (id, vec) bytes      |

A mismatch on any field refuses the load and falls back to the lexical
path with a structured error report. Pre-identity rows cannot silently
share an index with a named embedding model.

## Bounds

The default safety envelopes are declared in `denselocal.DefaultBounds()`:

| Bound         | Default | Purpose                                          |
| ------------- | ------- | ------------------------------------------------ |
| `MaxCorpus`   | 8192    | Refuses engines built with too many documents    |
| `MaxDim`      | 1024    | Refuses dimensions that exceed envelope          |
| `MaxTopK`     | 50      | Caps the number of hits a single call returns    |
| `MaxQueryLen` | 512     | Refuses oversized queries                        |

These are the regression ceilings cited in this document. Tightening them
in tests must use explicit overrides; production callers should rely on
the defaults rather than tightening them at runtime.

## Lexical fallback

When the index is missing, corrupt, or fails identity verification, the
arm falls back to a deterministic bag-of-words cosine over a corpus
captured at engine construction. Tie-breaking is by document id ascending
so two engines with the same corpus produce byte-equivalent hit lists.

The lexical path uses ASCII-alphanumeric tokenization. Camel-case
identifiers are treated as a single token (so "InvoiceTotal" matches as
"invoicetotal" not "invoice total"). This is intentional: it keeps the
fallback deterministic and reproducible across hosts and locales.

## Receipt contract

Every search call returns a `Report`:

```json
{
  "ok": true,
  "route": "lexical_fallback",
  "index_state": "missing_index",
  "corpus_vectors": 8192,
  "hits": [{"id": "billing.go", "score": 0.81, "route": "lexical"}],
  "query_sha256": "<sha256>",
  "model": "bag-of-words:v1",
  "scope": "<scope>",
  "content_digest": "",
  "identity_checked": true,
  "bounded_by": {
    "max_corpus": 8192,
    "max_dim": 1024,
    "max_top_k": 50,
    "max_query_len": 512
  }
}
```

The `query_sha256` field carries the query text as a hash, never as raw
text, so a leaked report cannot reveal the user's query.

## API surface

### CLI

```
sentra-code-memory dense-local [--root PATH] [--index PATH] \
                              [--scope NAME] [--model NAME] [--dim N] \
                              [--content-digest HASH] [--q TEXT] \
                              [--top-k N] [--max-corpus N] [--max-dim N] \
                              [--max-query-len N]
```

### JSONL

| Verb                 | Required                       | Optional                                                  |
| -------------------- | ------------------------------ | --------------------------------------------------------- |
| `dense_local_search` | `q`, `root`                    | `top_k`, `scope`, `model`, `index`, `max_corpus`, `max_dim`, `max_query_len` |

`scope` defaults to the canonical root when omitted. `model` defaults to
`bag-of-words:v1`, which selects the deterministic lexical fallback. Any
other model name requires an HNSW index built externally with that name.

## Evidence

- Pure-Go / no-network assertion: `TestNoNetworkImports` greps for
  `net/*` imports in the denselocal package source.
- Identity binding: `TestIdentityBindingPresent`, `TestLoadIndexIdentityMismatchFailsClosed`.
- Bounds enforcement: `TestBoundsEnforcedOnConstruction`,
  `TestTopKAndQueryBoundsEnforced`.
- Determinism: `TestLexicalFallbackDeterministic`,
  `TestScoreFallbackTieBreakByIDAscending`.
- Lexical fallback always available: `TestLexicalFallbackIsAlwaysAvailable`.
- Bench: `BenchmarkLocalLexicalThroughput`, `BenchmarkLocalLexicalAllocationsPerQuery`,
  `BenchmarkLocalLatencyP50P95`, `BenchmarkPersistAndReload`.

### Sample evidence from a darwin/arm64 workstation

```
BenchmarkLocalLexicalThroughput/corpus_256-12        50      65 µs/op
BenchmarkLocalLexicalThroughput/corpus_1024-12       50     274 µs/op
BenchmarkLocalLexicalThroughput/corpus_4096-12       50    1116 µs/op
BenchmarkLocalLexicalAllocationsPerQuery-12          50     125 µs/op   101 KB/op   929 allocs/op
```

Allocation counts stay below the 1200-per-query regression ceiling already
cited for the existing `dense.HNSW` benchmark.

## What is not implemented

The deferred `code_dense_rerank` catalog verb remains deferred per
ADR 0025; calling it returns a structured `error_code: "deferred"`
disclosure. Networked embedding models, HNSW + GPU acceleration, and
multi-tenant re-ranking are explicitly out of scope.
