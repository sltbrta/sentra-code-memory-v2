# hosted

**ONE** company-doc product runtime: `hosted.Client`.

## Store adapters (not dual engines)

| Adapter | Open | Role |
| --- | --- | --- |
| local_fs | CreateLocal / OpenLocal | Durable offline brain |
| memory | OpenMemory | Tests |
| product_neon | Open + schema | Product Neon tables |
| path2 | OpenFromEnv | ERB SMF corpus |

## Pipeline (residual company brain)

```text
ingest (hot):
  write chunks → retrieval_ready
  light memory seed: EnsureUtility + SetDocTexts + BindEpisode

async (default local_fs):
  EnqueueEnrichment → gardener.db
  gardener wave → WarmSidecars (doc2query / edges / context / dense)
  post-wave: memory.RunCortexMaintenance
    extract · edges · PageIndex TOC · global PR prior · community

ask:
  multi-arm IR (HotLex / lex / dense / structure / RRF / CE)
  + claim prefer · utility · global_pr · bipartite PPR
  + pageindex passages · RAPTOR/community inject
  + optional agent_memory (stm→mtm→ltm)
  → ground answer · dual-cite on conflict · bounded faithfulness gate
  → cite reinforce utility
```

Queue workers may emit receipt stubs; CLI/`memory` owns real cortex mutations.
Authority ACL Ask is a **separate** surface (`product-brain authority`).

## ERB budgets and cost accounting

Issue #278 (`budget.go`) bounds generation calls per request and stamps
`llm_budget` / `llm_tokens` diagnostics; issue #302 (`cost.go`,
`erb_prices.json`) attributes usage per stage/provider/model and stamps
`llm_usage` / `llm_cost` against a digest-pinned price table
(`OUROBOROS_ERB_PRICES` overrides). Counters only — sanitized-safe.
See `docs/stages/stage-09/evidence/enterprise-rag-bench/COST-DIAGNOSTICS.md`.

Issue #310 adds passive OpenTelemetry quality spans for ingest, answer,
retrieval, reranking, packing, synthesis, and citation verification. A typed
sanitizer emits only finite component/arm/outcome/provider/freshness categories,
capped counts, and capped estimated micro-USD from existing sanitized cost
diagnostics. Content, all application/evaluation IDs (including hashes), model
configuration, raw errors, secrets, and gold are excluded. Missing query stages
emit bounded `not_run` sentinels. The package uses the OpenTelemetry API only:
an unset global provider is a no-op, while an embedding process may call
`Client.SetQualityTracer` before serving requests.
See [quality tracing](../../../../docs/specs/brain/QUALITY-TRACING.md) for the
span/attribute contract, the representative baseline/traced p95 receipt
(`0.4041%` overhead), and the deployment boundary. Packing diagnostics report
the post-cap passage count actually emitted to synthesis.

Issue #283 (`retrieval_budget.go`) gives answer-time nested retrieval one
request-scoped depth/call/time envelope across exhaustive, agentic, and
corrective expansion. `retrieval_budget` diagnostics contain sanitized stage
counters and skip reasons only; bounded ExpandLite rescue remains enabled.

Issue #299 adds bounded provider resilience to synthesis: HTTP 429 responses
honor `Retry-After` through a process-local cooldown (default 30s, capped at
5m), and `OUROBOROS_ERB_HEDGE_DELAY_MS` may opt into one deadline-bounded hedge
to the next configured provider. Hedging is off by default, never expands the
provider roster, and caps concurrency at two attempts. Sanitized events are
recorded under `llm_budget.provider_health`.

Issue #290 (`provider_http.go`) shares one pooled HTTP transport across all
provider calls (synthesis, embed, rerank, multiquery, Qdrant, FAISS sidecar)
so repeated calls reuse warm TLS connections. Deadlines stay per request
(context deadline plus per-call client timeout), clients carry no cookie jar
or cached credentials, and provider ordering/cooldown/ledger behavior is
unchanged.

Issue #280 (`cohere.go`, `dense_query_batch.go`) batches the selected dense
query rewrites into request-scoped Cohere calls instead of launching one embed
HTTP request per rewrite. One provider request accepts at most 16 texts and
32 KiB of aggregate query input; every text is UTF-8-safely capped at 8,000
bytes. Larger plans split into sequential bounded batches. The first failed
batch stops further embed requests, preserves vectors from earlier successful
batches, and leaves dense misses to the existing lexical fallback.

ANN searches remain parallel and carry the unchanged `brain_id` Qdrant filter.
Their lists are reassembled in query order before RRF, and the existing hit
document/chunk/source metadata continues into citation passages. Diagnostics
report counts and bounds only under `dense_embedding_*`, `dense_ann_*`, and
`dense_status=ok|empty|partial_failure|error|skipped`; they never include query
text, principals, document IDs, ACL data, or citations. Recovery uses the same
contract with the `recovery_dense_*` prefix. Reproducible fixed-workload
latency/allocation results are recorded in the stage-09 evidence file
`EMBEDDING-BATCH-EVIDENCE.md` (not checked into this standalone extraction).

Issue #301 (`rerank_cache.go`) applies a blind, deterministic prefilter before
remote Cohere/ZeroEntropy/MLX reranking (default 64 candidates, hard maximum
96) and reuses complete pair scores in a bounded five-minute TTL/LRU. Cache
identity covers provider/model, brain/generation scope, embedding dimension,
the complete ACL context, query, and full document/chunk/source/text identity.
Incomplete identity disables reuse; stale entries, provider failures, partial
responses, and non-finite scores never become hits, while finite negative
scores remain valid. The ranked head and original tail preserve every
authorized `Passage`, including source and locator metadata used by grounding
and citations. Cohere/ZeroEntropy/MLX calls use the lesser of their fixed
provider cap and the remaining request deadline and fail closed before egress
when no time remains. Diagnostics expose capped counts, CE characters,
provider latency/timeout, fixed fallback reasons, and explicit unknown provider
cost when no pinned price is available; they contain no content, raw error,
identity, ACL, citation, or gold values. Reproducible fixed-workload and pinned
blind before/after evidence is recorded in the stage-09 evidence file
`RERANK-CACHE-EVIDENCE.md` (not checked into this standalone extraction).

Issues #313 and #324 (`faithfulness.go`) apply the final answer-level acceptance
gate.
It uses authorized company passages and grounded claims only, deterministically
filters unsupported claims, and otherwise abstains with empty claims/citations.
Accepted and repaired answers expose a calibrated `factual_consistency` score
beside citations; abstentions expose a distinct non-numeric `abstained` state.
The score is thresholded at 0.778 and can only tighten the faithfulness gate:
low-confidence or unavailable results use at most the same one-pass repair and
then abstain. They cannot override ACL, scope, citation, concrete-atom, or
abstention floors.
An opt-in repair can spend at most one slot from the existing request ledger;
there is no critic loop. Diagnostics are deterministic, sanitized, and ignore
gold/evaluator fields. See
`docs/stages/stage-09/evidence/enterprise-rag-bench/ANSWER-FAITHFULNESS.md`.
The gate is on by default; set `OUROBOROS_ERB_FAITHFULNESS=0` to roll back the
entire gate without assessment, citation rewriting, or a repair call.
Calibration evidence and explicit non-claims are in
`docs/stages/stage-09/evidence/factual-consistency-calibration-v1.json`.

## Entity-catalog recovery

Issue #303 indexes corpus-derived `path2_entities` once per immutable catalog
generation. Whole-token/prefix and packed-trigram postings keep large catalogs
off the query-time full-scan path. Catalog keys contained in a live seed retain
the original reverse-containment semantics; forward mid-token lookup uses a
bounded rare-trigram posting and can truncate on a very common gram. Every
candidate is rechecked with the original substring predicate. Exact rare
identifiers are retained ahead of generic recovery rewrites, after reserving a
bounded alias share. The first recovery query remains the user question.

Generate the offline projection with:

```bash
bazel run //services/brain/cmd/dump-entity-catalog -- \
  -o /hotlex/entity-catalog.gob -generation "$OUROBOROS_ERB_GENERATION_ID"
```

The generator reads entity names, aliases, and source document IDs only from
the selected brain's `path2_entities`; it accepts no questions, answers, or gold
document IDs. Writes fsync the temporary file, atomically rename it, and fsync
the containing directory before reporting durability. Hosted open prewarms the
decoded catalog and its indexes, reuses the resolved path and file identity for
30 seconds, reloads immediately on a path configuration or pinned generation
change, and rejects explicit brain/generation mismatches. Legacy catalogs
without a generation remain file-identity/TTL safe.
Decode failures are cached for the same 30-second identity-check TTL. Live SQL
samples single-flight per brain, so a cold brain does not hold a process-wide
lock while another brain spends its bounded load window.

`OUROBOROS_ERB_ENTITY_GOB` selects an explicit file; otherwise the loader checks
beside `OUROBOROS_ERB_HOTLEX_PATH`, then `/hotlex`. Catalog hits remain document
identity stubs until normal scoped hydration supplies corpus text/source
locators. They do not create citations or bypass downstream authorization.

Uncached path2 retrieval diagnostics expose `entity_catalog_index` with
cumulative candidate/match/fallback/truncation counters and steady-state index
shape (`keys`, token/gram posting counts, and `logical_payload_bytes`). The byte
metric is the deterministic key/token/posting payload only; it excludes Go map
buckets, slice headers, allocator rounding, and process RSS.
`fallback_skips_cumulative` or `truncations_cumulative` therefore report
possible recall loss rather than claiming full substring recall for bounded
large-catalog forward matches. Diagnostics contain counts only—never entity
text, document IDs, ACL data, or gold IDs.

## Security

Optional `productsec.Context` on Client; multi_principal deny before retrieve.

## Related

[continual](../continual/README.md) · [gardener](../gardener/README.md) ·
[memory](../memory/README.md) · [productsec](../productsec/README.md) ·
brain service [README](../../README.md)
