# Bounded grounded-query engine

Status: **[shipped] composed behind the authenticated local authority.** This
package answers grounded questions from the Stage 03 committed-Git
corpus with ACL-first lexical retrieval, one-generation pinning, canonical
hydration with digest and anchor verification, bounded evidence packing, and
fail-closed claim synthesis. It registers no Connect handlers, opens no
sockets, and persists nothing; the brain local-authority facade composes it
into the production command behind the authenticated gateway, and the Stage 04
semantic gate proves the full path end to end.

The engine consumes the Stage 03 occurrence projection
(`internal/codeindex`) and canonical revision facts (`internal/ingestion`)
through small ports and returns domain types. It deliberately emits no
protobuf: the gateway maps results onto the frozen `query.proto` boundary
exactly as Stage 03 mapped domain reads onto ingestion contracts.

## API

```go
engine, err := query.NewEngine(query.Config{
    Corpus: corpus, Authorizer: authorizer,
    Synthesizer: synthesizer, Clock: clock, Limits: query.DefaultLimits(),
    EvidenceAdmitter: evidenceAdmitter,
})
result, err := engine.Answer(ctx, query.Query{...})          // Ask
status, err := engine.Status(ctx, principal, sourceID)       // GetStatus
```

- `Answer` returns `(Result, error)`. The error is only `ErrInvalidInput`
  (malformed request), `ErrUnknownScope` (unknown, revoked, or unservable
  source or generation; the gateway maps it to the static
  `not_found_or_denied` error outcome), or a context error. Everything else —
  absence, denial, staleness, degradation, provider failure, projection
  absence — is a composed `Answer` with frozen disclosures, never an error.
- `Status` composes the authorized `GetStatus` view for the current complete
  generation: freshness, canonical-versus-indexed coverage, and projection
  state. It re-reads the current pin after the snapshot so a mid-read
  reconcile discloses `stale_disclosed` instead of reporting a superseded
  generation as current, and it reauthorizes immediately before emission, so
  a revocation landing mid-flight collapses the whole read to
  `ErrUnknownScope` with no generation metadata. Denied, unknown, and revoked
  sources share `ErrUnknownScope`.
- `Result` carries `Answer` (status, prose, claims, degraded reasons, token
  usage, and explicit factual-consistency scored/abstained/unknown disclosure),
  `Freshness` (pinned generation, state, ACL epoch, observation
  time), `Coverage` (indexed never exceeds canonical), and `Projection`.

### Ports

- `Corpus` serves generation-pinned `Snapshot` values: canonical manifest
  revisions plus a `ProjectionView` (`ready`/`rebuilding`/`absent`) holding
  the occurrence index and hydrated bytes when ready. Implementations return
  `ErrUnknownScope` for unknown or revoked scopes. Projection absence is a
  coverage fact, never deletion evidence.
- `Authorizer` evaluates the principal's current relationships at three
  checkpoints: `query` (before any corpus read), `hydrate` (before canonical
  byte access), and `emit` (after synthesis and before any result emission,
  catching mid-flight revocation on both Ask and Status). Any error is
  denial; denial never widens evidence.
- `EvidenceAdmitter` is required for the receipt-enforced boundary. It reads
  current canonical events and propagation receipts from a linearizable
  provider view. After a successful admission, the engine rechecks the current
  generation and admits once more at a fresh observation time; a generation,
  tombstone, or ACL change committed during a blocking first call therefore
  rejects final emission. Only frozen paths outside #316 may set the explicitly
  named `AllowLegacyUnadmittedEvidence` compatibility flag.
- `Clock` supplies observation time for reproducible answers.
- `Synthesizer` is the model-adapter boundary. `NewDeterministicSynthesizer`
  is the fixture/conformance adapter: a pure, byte-for-byte reproducible
  function that selects each evidence block's value line (first line holding
  the standalone keyword `return`, else the block's first line), cites its
  trimmed span, and renders the first string literal as the returned value.
  It proves pipeline, authorization, citation, and replay behavior and makes
  no answer-quality claim. `NewProviderSynthesizer` is the live adapter
  shape: one `ProviderClient.Complete` call bounded by a per-call context
  deadline (cooperative cancellation — the client must honor context, and
  the adapter discards any response arriving after the deadline), failing
  closed with a wrapped `ErrSynthesisFailed` and zero partial output on any
  provider error, timeout, or over-bound response, with no silent fallback to
  another provider or billing identity.
- `FactualConsistencyScorer` is optional and sees only retained verified claim
  text plus exact resolved citation spans. Its 64-claim/64-KiB/50-ms bounded
  result cannot mutate claims or citations. Nil, failure, timeout, invalid
  output, or budget exhaustion is disclosed as non-numeric `unknown`; answer
  abstention is a distinct non-numeric `abstained`. Numeric results require
  pinned scorer and calibration provenance. See
  [`internal/factualconsistency`](../factualconsistency/README.md).
  The hosted product answer boundary configures the repository's non-official
  calibration and fail-closed threshold. This generic Stage 04 engine keeps a
  nil scorer explicit as `unknown`; its other compositions must not inherit
  hosted calibration without population-specific evidence and enforcement.

## Retrieval and grounding rules

- **ACL-first.** Admission authorization is evaluated before any corpus
  read. An admission denial returns exactly `absent_support`, regardless of
  corpus, staleness, or projection state, so a denied principal cannot
  distinguish denial from genuinely absent support. Hydration reauthorization
  precedes canonical byte and digest work; emission reauthorization follows
  synthesis, so a mid-query revocation can only remove output. There is no
  `denied_support` vocabulary.
- **Candidates.** Exact path mentions select the named files and scope the
  query; a path-free query selects files whose syntax-aware definition
  spellings match query terms (case-insensitively). Lexically degraded files
  are selectable only by exact path and disclose `lane_degraded`.
- **Optional offline hybrid.** Lexical retrieval remains the default when no
  `DenseSearcher` is configured. `NewHybridEmbedDense(nil, bodies)` uses the
  deterministic bag-of-words fallback with no provider/model calls; configured
  dense results are fused with lexical candidates through the existing RRF
  path, with optional local lexical reranking.
- **Hydration.** Every entry is reverified against canonical facts: SHA-256
  of the hydrated bytes must match the manifest content digest, the Git blob
  identity must match, and the projection digest must agree. A mismatch
  discards the entry and surfaces `citation_verification_failed`.
- **Packing.** Entries pack in path order under the configured bounds
  (16 entries, 4 KiB per entry, 64 KiB aggregate ≈ the 16,000-token budget).
  Dropped facets and canonical-but-unindexed path mentions disclose
  `partial_coverage`; absence over a partial index always pairs
  `absent_support` with `partial_coverage`, so unindexed canonical revisions
  never masquerade as false absence or deletion.
- **Synthesis verification.** The engine re-resolves every proposed citation
  against the canonical pack: index, forward range, in-block coordinates, and
  recomputed supporting-text digest. A claim whose citation fails is removed
  on its own, exactly as the frozen text states; surviving claims are emitted
  with `citation_verification_failed` disclosed, prose is regenerated from
  the surviving statements, and only when no claim survives does the result
  abstain. Structural violations outside a single claim (over-bound prose or
  claim sets, malformed statements) surface `synthesis_unavailable`. Prose
  never carries a material span without a supported claim.
- **Status.** `answered` = claims and zero reasons; `partial` = claims and
  reasons; `abstained` = empty prose, zero claims, and at least one reason.
  Reasons are deduplicated, capped at eight, and emitted in the frozen
  vocabulary order.
- **Freshness.** Every answer pins exactly one generation. A superseded pin
  is always disclosed as `stale_disclosed`; `best_effort` and
  `complete_generation` may serve it with `stale_support` (v1 publications
  are atomically complete, so no wait applies), while `abstain_if_stale`
  abstains before any retrieval. A rebuilding or absent projection abstains
  `retrieval_unavailable` rather than reporting false absence.

## What the gateway leaf (L2) owns

This engine stays pure so L2 can compose it without surprises:

- **Conversation store.** Admission, idempotency records, and turn appends
  (migration 004) live in L2. The engine validates `IdempotencyKey` shape and
  echoes the server-authored `QueryID` but persists nothing; a cancelled
  context aborts without side effects.
- **Corpus adapter.** L2 adapts the local authority's published source
  (generation manifest, `codeindex.Snapshot`, hydrated files, readiness) into
  `query.Snapshot`, mapping revoked/unknown sources to `ErrUnknownScope` and
  restart/rebuild windows to `ProjectionRebuilding` or `ProjectionAbsent`.
- **Receipts and protobuf mapping.** L2 authors the `AskResponse` receipt,
  sets `AUTHORITY_CLASS_MODEL_PROPOSAL` on every claim, maps domain strings
  to the frozen enums, and wraps identifiers (`claim_id`, `evidence_id`,
  `source_revision_id`) in the shared `Identifier` message. The engine's
  invariants (citation shapes, bounds, status consistency, vocabulary,
  indexed ≤ canonical) are chosen so valid engine output always satisfies
  the boundary's CEL rules.
- **`ListSources` / `GetHistory`.** Pure L2 concerns (source registry and
  principal-scoped turn store); the engine is not involved.

## Verification

```sh
BAZEL_BIN=$(tools/build-spine/bootstrap-bazel.sh bazel)
"$BAZEL_BIN" test \
  //services/brain/internal/query:query_test \
  --cache_test_results=no --test_output=errors
```

The suite executes all twelve frozen grounding cases in
`tests/fixtures/stage-04/grounding/query-cases.json` over the reconstructed
Stage 03 mixed-P5 corpus (both generations projected by the real
`codeindex`), asserting exact statuses, citation ranges, supporting-text
digests, and degraded reasons, plus contract-shape invariants mirroring the
`query.proto` CEL rules and replay determinism. Boundary tests cover
malformed and oversized requests, unknown generations, principals without
relationships, hydration and emission denial, mid-query revocation, absent
and rebuilding projections, unindexed canonical facets, pack-budget
overflows, hydration integrity failures, hostile citation fabrication, and
every provider failure mode.
