# Chunking policy — versioned baseline, structure-aware alternatives, and benchmark contract

Status: **[shipped-additive]** — issue #332. The policy, receipts, and the
`chunk-eval` golden harness are implemented in
[`services/brain/internal/chunking`](../../../services/brain/internal/chunking/README.md)
and [`services/brain/cmd/chunk-eval`](../../../services/brain/cmd/chunk-eval).
The default ingestion path (`hosted.DocumentsToChunks`, whole-document
chunks) is **unchanged**; adopting a strategy there is a separate, gated
follow-up. Relates to #259, #287, #285.

## 1. Baseline and why alternatives are evaluated

The evaluated baseline is a **500-token target with at least 50-token
overlap**, applied as a sliding token window (`fixed` strategy, policy
`ouroboros-chunk` v1). It is a baseline, not an assumption:

- Fixed windows are source-agnostic and predictable, which makes them the
  honest reference point for cost and latency.
- They cut through structure: headings detach from their paragraphs, fenced
  code and tables tear mid-unit, chat turns split mid-exchange. Retrieval
  quality then depends on overlap rescuing the cut context, which spends
  index volume (embedding + serving footprint) on duplicated tokens.
- Therefore structure-aware variants are **evaluated against** the baseline
  per source kind, and parent-child chunking is evaluated as the
  precision-vs-context trade: small retrievable children, large expandable
  parents.

Strategies under evaluation (all policy-scoped, all deterministic):

| Strategy | Sizing (policy v1) | Boundary rule |
| --- | --- | --- |
| `whole_doc` | one chunk per document | legacy naive RAG baseline (`DocumentsToChunks` shape) |
| `fixed` | 500 target / 50 overlap | token window |
| `structure` | 500 target / block-granularity overlap (capped ≤ target/2 carry) | kind-aware blocks, below |
| `parent_child` | children 125/25; parents 1000/100 | token windows, children linked to max-overlap parent |

Structure-aware block rules by source kind:

| Kind | Blocks never torn (unless a single block exceeds target) |
| --- | --- |
| `prose` | paragraphs; headings travel with the following text |
| `code` | fenced code blocks; prose between fences splits on blank lines |
| `table` | consecutive pipe-delimited rows; oversized tables split at row boundaries |
| `slides` | one slide per separator rule (`---`) |
| `chat` | one speaker turn per block; wrapped lines join the turn |

## 2. Versioning contract

Every chunk is stamped with the full policy fingerprint:

- `policy_id` (`ouroboros-chunk`) and `policy_version`;
- `tokenizer_id` — version 1 is `ouroboros-ws-1`, a deterministic
  whitespace-word model with byte offsets. It is a documented approximation;
  swapping to a model tokenizer changes boundaries, so it requires **both** a
  tokenizer version bump and a policy version bump;
- strategy, kind, and seq.

Rule: **any change that moves a chunk boundary bumps `policy_version`.**
Chunk IDs embed `strategy.vVERSION`, so a version bump never silently
overwrites prior receipts; re-chunking produces a new, attributable identity
space (this is the re-chunked projection substrate that #287 benchmarks).

## 3. Receipt contract (offsets, parents, rebuild identity)

Every `chunking.Receipt` preserves:

- **Source offsets** — byte offsets into `SourceDocument.Source()` (title
  prefix + body, the exact string chunks slice), with the invariant
  `Source[Start:End] == Text`;
- **Parent identity** — parent-child children carry `parent_id`; parents are
  recorded in the receipt ledger even when they are not indexed;
- **Content hash** — sha256 of the chunk text;
- **Rebuild identity** — `Chunk` is a deterministic function: identical
  documents + policy always yield byte-identical receipts, so rebuilds are
  diffable and the identity tuple
  `(policy_id, policy_version, tokenizer_id, document_id, seq)` is stable.

`chunk-eval` verifies all of the above on every retrieved chunk (citation
integrity, §4); a store round-trip that trims, re-encodes, or re-parents text
fails the check.

## 4. Evaluation harness: `chunk-eval`

Retrieval-only, offline, deterministic. It reuses the existing product
contracts instead of forking them: ingestion through
`ChunkStore.UpsertChunks` (`hosted.NewMemoryChunkStore`), retrieval through
the `hosted.ProjectChunks` BM25 HotLex projection.

```bash
go run ./services/brain/cmd/chunk-eval \
  --fixtures services/brain/cmd/chunk-eval/testdata/golden \
  --top-k 8 --report report.json
```

### Golden fixtures

- 12 documents × 5 sections = **60 golden queries** (≥ 50), covering all five
  source kinds, committed under `testdata/golden/` and pinned to a
  deterministic generator (`TestGoldenFixturesAreCurrent`; regenerate with
  `OUROBOROS_CHUNK_EVAL_REGEN=1`).
- Gold is **policy-agnostic**: each query's gold is a document plus a
  corpus-unique needle token. Any strategy's chunk that contains the needle
  inside the gold document counts as a hit, which is what makes cross-strategy
  comparison fair.
- The probes are synthetic marker probes, not real enterprise questions; the
  harness measures the chunking layer, and ERB remains the external validity
  check (§5).

### Reported metrics per strategy

| Metric | Meaning |
| --- | --- |
| `hit_rate_at_k`, `doc_hit_rate_at_k` | any gold chunk / any gold document in top-k |
| `mrr` | mean reciprocal rank of the first gold chunk |
| `ndcg_at_k` | binary relevance; the ideal uses the true relevant-chunk count (overlap legitimately duplicates a needle across adjacent chunks) |
| `citation_integrity` | fraction of retrieved chunks whose receipt offsets/hash/parent links verify; violations listed |
| cost proxies | chunks indexed, index tokens, mean chunk tokens, expansion ratio vs source (retrieval-only: no paid calls, so cost is index volume) |
| latency | chunk ms, ingest ms, query p50/p95 ms |

## 5. ERB-blind comparison hooks (diagnostics ≠ official)

- Reports stamp `score_class: "diagnostic"` and `official: false`
  unconditionally. Official ERB scores come exclusively from the pinned
  EnterpriseRAG-Bench harness with `OUROBOROS_ERB_OFFICIAL_JUDGE`; if that
  env is present, `chunk-eval` records it (`official_eval_env`) and still
  refuses to emit anything official.
- `--blind --blind-key <path>` runs an ERB-blind comparison: strategies are
  labeled `arm_a..arm_d` in report order, policy fingerprints are stripped
  from the report, and the label→strategy mapping is written only to the key
  file. Judges can compare arms without knowing which arm is the baseline.
- Gold never feeds retrieval behavior: needles are used only for post-hoc
  scoring, mirroring the ERB blind-plan separation between diagnostics and
  official scoring.

## 6. Rollout and reversibility

- Additive only: no existing ingestion/retrieval code paths changed; the
  legacy whole-document behavior remains the product default.
- To adopt a strategy later: map receipts to `hosted.ChunkWrite`
  (`DocumentID`, `ChunkID`, `Text`, `SourceURI`) and feed the existing
  `BurstUpsert`/delta paths — the write contract is unchanged. Re-chunking an
  existing brain is a rebuild under a new policy version; receipts give the
  old↔new identity mapping.
- Reversibility: revert the #332 commit. Nothing outside the new packages,
  the harness, this document, and the `just ci` test list is touched.
