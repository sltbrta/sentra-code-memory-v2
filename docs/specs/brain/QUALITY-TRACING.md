# OpenTelemetry quality tracing

Issue #310 adds passive OpenTelemetry API instrumentation to the product-owned
company-doc pipeline in `services/brain/internal/hosted`. It is operational
diagnostics, not an official benchmark score, audit log, or new evaluation
input.

## Trace shape

The public operations emit these finite span names:

| Span | Boundary |
|---|---|
| `ouroboros.brain.answer` | one `AnswerOpts` call |
| `ouroboros.brain.ingest` | one batch or burst upsert |
| `ouroboros.brain.retrieve` | each initial or bounded answer-time retrieve |
| `ouroboros.brain.rerank` | each bounded cross-encoder/lexical rerank |
| `ouroboros.brain.pack` | each bounded evidence-to-prompt packing pass |
| `ouroboros.brain.synthesize` | each ledger-admitted synthesis call |
| `ouroboros.brain.citations` | each grounding/citation verification pass |

Existing context deadlines, retrieval expansion caps, rerank candidate caps,
prompt caps, generation ledgers, and citation/claim limits bound their work and
lifetime. Tracing starts no pipeline work of its own.

An answer trace must remain structurally useful on early return. Immediately
before the root span ends, each required query stage that did not run gets one
zero-work sentinel span with `outcome=not_run` and an unset OpenTelemetry
status. A stage that ran and failed instead records a finite failure category
(`error`, `timeout`, or `canceled`) and error status. Raw errors are never span
descriptions or events.

Native OpenTelemetry trace/parent context is the only request correlation.
Request, question, brain, tenant, session, document, chunk, citation, and other
application IDs are not copied or hashed into telemetry. Finite
`pipeline.component` and `pipeline.arm` values correlate stage behavior across
traces without creating an identifier side channel.

## Sanitized attribute contract

`qualityReceipt` is the sole input to the attribute builder. Stage adapters
read only allow-listed diagnostic facts, collapse strings to finite enums,
clamp counts, convert estimated USD to capped integer micro-USD, and discard
the source map before setting attributes. No attribute is copied directly from
an arbitrary diagnostics map.

Each span carries at most nine attributes. String values are finite enums no
longer than 64 bytes. Counts are clamped to `0..1000`; estimated cost is clamped
to `0..1,000,000,000` micro-USD. No span events or links are emitted.

| Span | Sanitized attributes (when available) |
|---|---|
| answer | component, mode, outcome, provider, freshness, estimated micro-USD, citation count, claim count, abstained |
| ingest | component, arm, outcome, freshness, input count, output count |
| retrieve | component, arm, outcome, freshness, input/output count, cache hit |
| rerank | component, arm/provider category, outcome, input/output count |
| pack | component, arm, outcome, freshness, input/output count |
| synthesize | component, ledger-stage arm, outcome, provider category, freshness, input/output count |
| citations | component, arm, outcome, freshness, grounding status, input count, citation count, claim count, abstained |

Packing `input_count` is the candidate passage count at entry. Packing
`output_count` and synthesis `input_count` are the actual passage inputs emitted
after the 16-document and 4-conversation-snippet prompt caps, so truncated
candidates are not reported as synthesis inputs.

Provider categories are the known product adapters (`openai`, `gemini`,
`openrouter`, `groq`, `mlx`, `cohere`, `zeroentropy`, `lexical`, `extractive`,
and `abstain`); every other value is `unknown`. Model names and provider
configuration are excluded. Cost comes only from the already-sanitized
`llm_cost.total_cost_usd` diagnostic and is absent when usage or pricing is
unavailable. Freshness is a finite state derived from sanctioned freshness,
cache, and recency-pack diagnostics; it is never a generation or revision ID.

## Privacy and evaluation boundary

The instrumentation must never export:

- question, answer, history, prompt, passage, claim, quote, or source content;
- request, question, brain, tenant, principal, session, document, chunk,
  citation, generation, revision, run, or evaluation IDs, including hashes;
- URLs, model names, provider configuration, credentials, raw errors,
  exception events, or response bodies;
- gold document IDs, counts, labels, gold-assisted flags, judge data,
  official scores, or other gold-derived diagnostics.

Tests place content, secret, ID, model/error, and gold canaries in every input
surface and inspect span names, statuses, descriptions, events, links,
attribute keys, and values. Aggregate bounded counts may reflect normal
pipeline output shape, but no gold-specific metric is read by the telemetry
sanitizer.

## Overhead evidence

The checked-in
[`quality-tracing-overhead.json`](../../stages/stage-09/evidence/quality-tracing-overhead.json)
is a paired representative-workload receipt, validated against its closed
[`quality-tracing-overhead.schema.json`](../../stages/stage-09/evidence/quality-tracing-overhead.schema.json).
It exercises the product `Client.AnswerOpts` path over a 24-document
memory/SQLite FTS corpus with `top_k=16` and extractive synthesis. Each of three
independent runs warms both variants, then alternates 2,500 baseline requests
(global OpenTelemetry no-op provider) with 2,500 traced requests (always-sampled
recording SDK, no exporter).

The median independent-run p95 was 2,577,792 ns baseline and 2,588,209 ns with
tracing: `overhead_pct=0.4041`, below the `<2%` acceptance bound. Reproduce the
recorded measurement with:

```bash
go test ./services/brain/internal/hosted -run '^$' \
  -bench '^BenchmarkQualityTracingRepresentativeWorkload$' \
  -benchtime=2500x -count=3
```

Wall-clock timing is evidence, not a runtime test threshold: scheduler and
shared-runner noise would make that gate flaky. The stable test instead
strictly decodes the receipt, validates its schema identity, p95 aggregation,
arithmetic, `<2%` recorded claim, and non-runtime-gate marker. The separate
deterministic structural test evaluates 140 fixed span shapes and enforces
`p95(attribute writes per span) = 9` against the production cap of 9.

## Optional provider boundary

The hosted library depends only on the OpenTelemetry API at runtime. It does
not create an SDK, exporter, endpoint, background worker, or network client.
With no global provider installed, OpenTelemetry uses its no-op implementation
and behavior is unchanged. An embedding process may install a global provider
or call `Client.SetQualityTracer` before serving concurrent requests.

Exporter endpoints, authentication, resource attributes, sampling, retention,
and shutdown/flush belong to the embedding process. They must not be copied
into span attributes. Export failure cannot fail, retry, or otherwise alter
ingestion, retrieval, reranking, packing, synthesis, citation verification, or
answer results.
