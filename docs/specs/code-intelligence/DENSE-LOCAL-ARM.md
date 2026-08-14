<!-- markdownlint-disable MD060 MD040 -->

# Local Lexical Code Retrieval Arm (issue #59)

## Scope and non-goals

This document captures the optional, opt-in local-first code retrieval arm
added in response to issue #59. The `denselocal` package serves a
deterministic bag-of-words cosine ranking over a corpus captured at engine
construction, with policy guard-rails that make local-first retrieval safe
to call: identity labels on every receipt, bounded corpora and queries, and
an explicit no-network contract.

**The arm is lexical-only.** No ANN/HNSW serving is implemented: a query
never becomes a vector here and no index file is loaded or served. Shipping
an HNSW-serving arm would require a query embedder with a persisted,
identity-bound vocabulary; that work remains deferred (see "What is not
implemented"). Earlier revisions of this document described HNSW serving;
those claims were removed because the route was never exercised.

This surface never imports a network package and never changes the
defaults of any existing code operator. The deferred
`code_dense_rerank` catalog verb (ADR 0025) covers the credentialed,
hosted, multi-tenant dense/rerank lane; this surface is the local,
deterministic counterpart that does not require a backend.

## Identification

Every engine is constructed with two identity labels that are carried in
every receipt:

| Field   | Source of truth                             |
| ------- | ------------------------------------------- |
| `scope` | the generation/repo the corpus belongs to   |
| `model` | the retrieval model name (`bag-of-words:v1`)|

Construction fails closed when either label is empty, so a future vector
arm cannot silently share this arm's receipts. Receipts never carry raw
query text — only its SHA-256.

## Bounds

The default safety envelopes are declared in `denselocal.DefaultBounds()`:

| Bound         | Default | Purpose                                          |
| ------------- | ------- | ------------------------------------------------ |
| `MaxCorpus`   | 8192    | Refuses engines built with too many documents    |
| `MaxDim`      | 1024    | Declared ceiling for any future vector arm       |
| `MaxTopK`     | 50      | Caps the number of hits a single call returns    |
| `MaxQueryLen` | 512     | Refuses oversized queries                        |

These are **hard ceilings**, not request-controlled parameters. A request
(CLI flag or JSONL field) may only tighten them; values above the defaults
are clamped down and the enforced envelope is reported in every receipt's
`bounded_by` block. Corpus loading is bounded before files are read: at
most `MaxCorpus` files are opened, each capped per-file, so a runaway
corpus cannot degrade the service. Tightening them in tests must use
explicit overrides; production callers should rely on the defaults.

## Ranking

The arm tokenizes the corpus once at construction (lowercase, ASCII
alphanumeric) and ranks by bag-of-words cosine. Camel-case identifiers are
treated as a single token (so "InvoiceTotal" matches as "invoicetotal" not
"invoice total"). Tie-breaking is by document id ascending so two engines
with the same corpus produce byte-equivalent hit lists. This is
intentional: it keeps the arm deterministic and reproducible across hosts
and locales.

## Receipt contract

Every search call returns a `Report`:

```json
{
  "ok": true,
  "route": "lexical",
  "hits": [{"id": "billing.go", "score": 0.81, "route": "lexical"}],
  "query_sha256": "<sha256>",
  "model": "bag-of-words:v1",
  "scope": "<scope>",
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
sentra-code-memory dense-local [--root PATH] [--scope NAME] [--model NAME] \
                              [--q TEXT] [--top-k N] \
                              [--max-corpus N] [--max-dim N] \
                              [--max-query-len N]
```

### JSONL

| Verb                 | Required                       | Optional                                                  |
| -------------------- | ------------------------------ | --------------------------------------------------------- |
| `dense_local_search` | `q`, `root`                    | `top_k`, `scope`, `model`, `max_corpus`, `max_dim`, `max_query_len` |

`scope` defaults to the canonical root when omitted. `model` defaults to
`bag-of-words:v1`, the only model this arm serves. `top_k` above 50 is
clamped to 50; `max_*` values above the defaults are clamped to the
defaults. Hit IDs are slash-separated paths relative to `root`; responses do
not expose absolute filesystem paths.

## Evidence

- Pure-Go / no-network assertion: `TestNoNetworkImports` greps for
  `net/*` imports in the denselocal package source.
- Identity binding: `TestIdentityBindingPresent`.
- Bounds enforcement: `TestBoundsEnforcedOnConstruction`,
  `TestTopKAndQueryBoundsEnforced`, `TestDenseLocalCLIHardBounds`.
- Determinism: `TestLexicalDeterministic`,
  `TestScoreTieBreakByIDAscending`.
- Receipt shape: `TestReportIncludesBounds`,
  `TestQueryHashNeverLeaksPlaintext`.
- Bench: `BenchmarkLocalLexicalThroughput`, `BenchmarkLocalLexicalAllocationsPerQuery`,
  `BenchmarkLocalLatencyP50P95`.

### Sample evidence from a darwin/arm64 workstation

```
BenchmarkLocalLexicalThroughput/corpus_256-12        50      65 µs/op
BenchmarkLocalLexicalThroughput/corpus_1024-12       50     274 µs/op
BenchmarkLocalLexicalThroughput/corpus_4096-12       50    1116 µs/op
BenchmarkLocalLexicalAllocationsPerQuery-12          50     125 µs/op   101 KB/op   929 allocs/op
```

## What is not implemented

The deferred `code_dense_rerank` catalog verb remains deferred per
ADR 0025; calling it returns a structured `error_code: "deferred"`
disclosure. Dense/ANN serving (HNSW or otherwise), networked embedding
models, GPU acceleration, and multi-tenant re-ranking are explicitly out
of scope. A future vector arm must land with a persisted, identity-bound
vocabulary and its own spec revision before any `index` serving surface is
exposed.
