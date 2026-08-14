# bench-code: offline retrieval benchmark gate

`just bench-code` runs a deterministic, offline retrieval-quality benchmark on
the checked-in `qafixture` corpus (issue #48). It needs no credentials, no
network, and no external services. It gates on regression thresholds and
records an artifact digest plus a multi-client smoke matrix.

## Run

```sh
just bench-code          # gate + record artifact to .local-agent-ci/logs/
just bench-code-json     # print the JSON artifact to stdout
```

The gate exits non-zero when a threshold is violated. The artifact is written
to `.local-agent-ci/logs/bench-code-report.json` (gitignored) and a concise
summary is printed.

## What it measures

For each retrieval probe on the fixture, through the `codeserve` protocol:

- **hit@1 / hit@5 / hit@10** — whether an expected file appears in the ranked
  top-k of `code_search`.
- **precision** — expected hits divided by returned hits.
- **latency** — per-probe and P95. Recorded, never asserted directly; the P95
  threshold only guards against pathological hangs.
- **context/token savings** — served normalized response bytes versus the
  naive "read the whole tree" baseline (`scmbench.EstimateTokens`).
- **failure classification** — each probe is `hit`, `miss` (hits returned but
  none expected), `empty` (no hits), or `error` (verb failed), aggregated in
  the report.

## Retrieval lanes

Benchmark claims must state which lane produced them. The standalone product
implements exactly one:

- `local_heuristic` — the lexical/heuristic codecrawl lane. **Measured.**
- `dense_reranked` — dense vectors plus cross-encoder rerank. **Deferred**,
  never measured or substituted (see `code_dense_rerank` disclosure).
- `compiler_authority` — Tree-sitter/SCIP/LSP authority. **Deferred.**

The report and baseline carry a `lane` field so a number is never silently
attributed to a lane that is not implemented.

## Fixture and baseline

The corpus is checked in at
`services/brain/internal/scmbench/testdata/qafixture/`: a small multi-package
Go tree (auth, db, api, util, models, config) with distinct symbols and a few
lexical distractors. The checked-in probe set (issue #48, expanded under
issue #54) carries 24 probes — 19 exact-identifier queries (expected at
rank 1) plus 5 multi-word lexical queries — spanning every fixture package so
hit@k is measured across the whole corpus rather than a seven-file slice.

The baseline is checked in at
`services/brain/internal/scmbench/testdata/qafixture-baseline.json`. It records
the artifact digest, measured hit rates, token-savings ratio, and thresholds.
The run compares the computed digest against the baseline and reports
`baseline match`. A digest change marks a deliberate retrieval diff; regenerate
the baseline with:

```sh
go run ./services/brain/cmd/bench-code \
  --write-baseline services/brain/internal/scmbench/testdata/qafixture-baseline.json
```

The hard gate is the threshold check (hit@1/5/10 floors, minimum token
savings, zero errored probes); the digest is a change detector, not a floor.

## Multi-client smoke matrix

Each run also exercises a probe set (ping, catalog, code_index, code_search,
savings_summary, and a deferred verb) across all three surfaces — direct
JSONL/`codeserve.Handle`, HTTP `/dispatch`, and MCP `tools/call` — and requires
all three to agree on the outcome and error code. This keeps the CLI/HTTP/MCP
equivalence honest, including the deferred-verb disclosure.

## Determinism

Hit@k, precision, failure classes, and token totals are path-normalized and
latency-free, so the digest is stable across machines and checkouts. Latency is
reported for context but excluded from the digest and the savings math.
