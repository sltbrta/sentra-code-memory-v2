# dense

In-process dense embedding bag / ANN helpers used by residual hybrid and
gardener dense jobs. Rebuildable projection material — not authority.

| Type | File | Notes |
| --- | --- | --- |
| Memory bag | `store.go` | Tests / tiny corpora |
| **HNSW (FAISS-class local)** | `hnsw.go` | Pure Go, no CGo; `dense=faiss` default in residual |
| FAISS CGo hook | `faiss_cgo.go` | Build `-tags faiss` when libfaiss linked |

HTTP FAISS sidecars live in `hosted/dense_faiss.go`
(`OUROBOROS_BRAIN_DENSE_URL`).

## Local ANN contract

The SQLite residual substrate keeps vectors and evidence metadata in `dense.db`
but serves queries from a scope-hashed `dense.<scope-digest>.ann` file. The ANN
header pins source scope, embedding model identity, dimensions, and a canonical
SHA-256 digest over vector IDs, vector bytes, DSIDs, chunk IDs, and source URIs.
A same-count replacement therefore cannot pass restart validation.

Publication is build-then-swap. A committed SQLite snapshot is indexed into a
temporary file, the file is fsynced and closed, renamed over the serving path,
and the parent directory is fsynced. Only then does the in-process state become
`ready`. A failure leaves the new ANN invisible and marks it stale; queries may
use only the bounded exact fallback described below. This makes every visible
post-crash file either the prior complete generation, the new complete
generation, or absent; its content digest decides whether it matches SQLite.

Query work is bounded by the HNSW `ef`/neighbor candidate budget. Corpora with
at most 512 vectors retain deterministic exact cosine semantics. When a
model-pinned SQLite projection has no ANN file, the same exact route is
permitted only up to 512 vectors; larger missing/corrupt/incompatible indexes
return a typed error and receipt-safe diagnostics instead of scanning the
corpus. Pre-identity rows marked `legacy` are not assumed compatible with a
named embedding model. Ranking is score descending with document ID ascending
as the tie-break.

`OUROBOROS_BRAIN_DENSE_SEARCH_MODE=auto|exact|ann` explicitly selects local
HNSW serving. `auto` retains exact semantics through 512 vectors; `exact` forces
the in-memory truth route, and `ann` forces bounded approximate search even for
small corpora. Missing/corrupt/stale indexes still use only the separately
bounded SQLite fallback; the override cannot turn a missing index into an
unbounded SQL scan.

The hermetic fixed-corpus gate uses 256, 1,024, and 2,048 deterministic
32-dimensional corpora. For every query it computes exact top-k and measures
true ANN recall@k as set overlap, plus p50/p95 latency, allocations/query,
estimated index memory, and durable file bytes. `product-brain dense-bakeoff`
runs the same exact-vs-ANN methodology at configurable sizes (default 256,
2,048, and 8,192). The hermetic gate requires recall@10 >= 0.75, ANN p95 <=
500 ms (including race instrumentation), <= 1,200 allocations/query, and <=
1 KiB/vector each for estimated index memory and disk. These are regression
ceilings, not fleet SLO claims;
provider-backed Qdrant/FAISS/pgvector require their own receipts.
