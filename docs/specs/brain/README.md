# Brain ingestion, memory, and query specification

Status: **[partial]** — domain ambition below; **shipped residual** is
`product-brain` (`hosted` + `memory` cortex + gardener + authority substrate).
This document remains the long-form **domain** contract (evidence model, agents,
federation ambition). Live pipeline truth:
[ARCHITECTURE.md](../../architecture.md),
[memory/README.md](../../../services/brain/internal/memory/README.md),
[REMAINING-GAPS.md](../../roadmap/REMAINING-GAPS.md).
Chunking contract: [CHUNKING-POLICY.md](CHUNKING-POLICY.md) (versioned 500/50
baseline, structure-aware alternatives, `chunk-eval` golden harness — issue
332).
Projection propagation SLOs: [PROJECTION-SLOS.md](PROJECTION-SLOS.md)
(freshness/deletion/permission-change targets per projection, fail-closed
offline verifier — issue #316).

Do **not** read “no runtime exists” — residual company-doc and authority RPC
paths ship; full enterprise profile (OpenFGA production, multi-host mesh, …)
does not.

## 1. Authority model

One causal graph links company evidence, decisions, conversations, code, tests,
docs, traces, and proposed changes, while preserving these authorities:

- event ledger owns operational history;
- immutable source revisions and encrypted blobs own evidence;
- semantic graph owns derived claims/relations as projections;
- Git owns executable source history;
- conversation history owns what was said, not what is true;
- OpenFGA owns permission decisions;
- policy-gated executors own external effects.

Typed edges include `governs`, `implements`, `derived_from`, `supports`,
`contradicts`, `invalidates`, `supersedes`, and `verified_by`.

## 2. Evidence and provenance

```ts
type SourceRevision = {
  sourceObject: SourceObjectRef;
  upstreamRevision: string;
  contentDigest: Digest;
  mediaType: string;
  byteLength: bigint;
  observedAt: Instant;
  validInterval: Interval;
  deletionState: "active" | "deleted";
  aclEpoch: bigint;
  encryptionKeyEpoch: bigint;
  provenance: Provenance;
};

type EvidenceAnchor =
  | { kind: "bytes"; start: bigint; end: bigint }
  | { kind: "text"; rendition: ArtifactRef; startByte: bigint; endByte: bigint }
  | { kind: "page"; page: number; bounds: NormalizedBox }
  | { kind: "table"; table: number; row: number; column: number }
  | { kind: "audio"; startMs: bigint; endMs: bigint; speaker?: EntityRef }
  | { kind: "video"; startMs: bigint; endMs: bigint; frames?: FrameRange; bounds?: NormalizedBox }
  | { kind: "code"; commit: GitOid; symbol?: SymbolRef; range: SourceRange };
```

Every transformation records exact inputs and outputs, digests, responsible
principal, tool/model/adapter, policy/configuration versions, and time. The
compact internal provenance model maps to W3C PROV Entity/Activity/Agent.

Grounded answers also expose answer-level factual-consistency as a typed
`scored`, `abstained`, or `unknown` result. Numeric scores pin the exact scorer
version and calibration artifact digest; abstention and unknown are never
interpreted as numeric zero. Hosted uses a small held-out synthetic,
non-official calibration whose score may only tighten the final faithfulness
gate. The bounded implementation, metrics, and explicit non-production posture
are documented in
[`services/brain/internal/factualconsistency`](../../../services/brain/internal/factualconsistency/README.md).

## 3. Continual ingestion

Use a snapshot-plus-delta connector protocol:

1. Authenticate connector and authorize exact tenant/source scope.
2. Treat signed webhooks as low-latency hints with delivery IDs; periodically
   reconcile against provider snapshots/delta cursors.
3. Pull opaque paginated deltas. Advance a cursor only after every page and
   canonical receipt commit.
4. Stage/hash/encrypt raw bytes; commit immutable source revision, evidence,
   ACL state, receipt, and outbox in the defined transaction protocol.
5. Execute the deterministic fast pass and publish queryable baseline lanes.
6. Debounce and burst selective enrichment outside the query path.
7. Publish only internally complete projection generations.
8. Process deletion and ACL revocation ahead of ordinary enrichment.
9. Reconcile periodically; absence in a failed/partial snapshot is not deletion.

Freshness is configurable by user/company while retaining the same tiers:

- cheap structural/exact/lexical/time deltas immediately;
- embeddings and bounded deterministic extraction promptly;
- LLM enrichment, aliases, typed relations, summaries, doc2query, contextual
  embeddings, and consolidation asynchronously on Modal/company workers;
- a query may request a freshness barrier and wait, return partial/degraded, or
  abstain according to policy.

Large media uses immutable composite manifests and parallel per-segment
renditions. Partial extraction is reported as partial coverage and cannot become
a complete generation.

## 4. Memory agents

Memory is maintained by bounded agents and deterministic reactions:

- **ingestion agent:** classifies and routes new evidence;
- **linking agent:** proposes entities, relations, aliases, and code/company
  edges;
- **contradiction agent:** finds incompatible claims and time/policy
  differences;
- **propagation agent:** maintains dependencies needed for Cascade and Absence;
- **deletion agent:** calculates lineage, denies immediately, and verifies
  purge;
- **gardener:** consolidates, summarizes, decays, and proposes shortcuts;
- **retrieval agent:** selects authorized brains/lanes under a token/cost
  budget;
- **critic:** challenges unsupported memories and promotion candidates.

Deterministic code owns identities, versions, ACL, graph traversal bounds,
dependency propagation rules, tombstones, receipts, and promotion. Agents
propose semantic content and maintenance actions. Every reaction is idempotent,
versioned, budgeted, and loop-bounded.

## 5. Conversation history and authority

Conversation events are immutable and private by default:

```ts
type ConversationEvent = {
  turnId: TurnId;
  sessionId: SessionId;
  owner: PrincipalId;
  visibility: VisibilityRef;
  role: "user" | "assistant" | "system" | "tool";
  content: ContentPartRef[];
  parentTurnIds: TurnId[];
  sequence: bigint;
  createdAt: Instant;
  supersedes?: TurnId;
  authority: AuthorityClass;
  sourceEvidence: EvidenceRef[];
};
```

Authority classes are `direct_source`, `machine_observation`,
`approved_decision`, `user_directive`, `user_assertion`, `model_proposal`, and
`derived_summary`.

- A user is authoritative about their instruction/preference.
- A user assertion proves the user said it, not that it is factually true.
- Assistant text is always a model proposal; repetition cannot promote it.
- Tool/test/compiler/Git/connector results are machine observations only with
  immutable evidence.
- A summary cannot become more authoritative, visible, or durable than its
  sources.
- An approved decision governs desired state but does not prove implementation.

Editing appends a superseding turn. Deletion appends a tombstone and launches
lineage purge/crypto-shred. User-scoped cross-session search includes the
owner's raw sessions and promoted personal memory. Team sessions require
explicit sharing; raw conversations are not globally searched.

## 6. Grounded query

```ts
type BrainQuery = {
  query: string;
  principal: AuthenticatedPrincipal;
  sessionId?: SessionId;
  scopes: BrainScope[];
  includeOwnPriorSessions: boolean;
  includeSharedSessions: boolean;
  validAt?: Instant;
  knownAt?: Instant;
  repoRevision?: GitOid;
  tokenBudget: number;
  freshness?: FreshnessRequirement;
};

type GroundedAnswer = {
  status: "answered" | "partial" | "abstained";
  prose: string;
  claims: GroundedClaim[];
  evidence: EvidenceDescriptor[];
  validAt: Instant;
  knownAt: Instant;
  consistencyWatermark: bigint;
  aclEpoch: bigint;
  routingReceipt: RoutingReceipt;
  coverage: number;
  degradedReasons: StableReason[];
  tokenAccounting: TokenAccounting;
};
```

Pipeline:

```text
authenticate → append user turn → authorize brain cards → route
→ exact/lexical/temporal/graph/conversation retrieval
→ post-authorize and hydrate canonical evidence
→ pack under budget with direct evidence floor
→ generate claims → structurally verify citations
→ entailment/check or abstain → append assistant turn → render
```

Every material prose span belongs to a claim and cites authorized anchors.
High-risk numerical/security claims prefer exact structured or quoted evidence.
Fabricated, deleted, stale, or inaccessible citations force partial/abstained.

## 7. Federated hive mind

Do not query every brain. Maintain ACL-protected `BrainCard` projections with
non-sensitive scope, modality counts, freshness, time coverage, size, topical
sketch, and expected cost.

1. Filter cards by tenant, identity, relationships, region, and requested scope
   before query fan-out.
2. Rank eligible brains using deterministic scope/lexical features, size, and
   expected relevance.
3. Query a small initial set and expand only if coverage/confidence is
   insufficient.
4. Send attenuated short-lived capabilities; remote brains authorize locally.
5. Merge evidence references centrally, not unrestricted remote prose.
6. Cache source-bound encrypted results by principal capability, ACL epoch,
   query policy, time watermark, and source generation.

Trickle-up shares explicitly promoted derived claims, not unrestricted raw
summaries. The audience of a derived artifact is the intersection of supporting
evidence audiences.

## 8. Company events informing code

```text
ApprovedDecision or EvidenceChanged
→ impact analysis across code/docs/tests/contracts/schema/config/infra/owners/deploy
→ ChangeIntent
→ exact-base transactional ChangeSet
→ isolated execution
→ compiler/LSP/tests/security/docs verification
→ independent cross-family review
→ policy/human promotion
```

Scoped auto-approval may create the worktree, candidate, and verification
evidence. It cannot silently change checked-out or production code.

## 9. Mandatory evaluation

- MEME Cascade, Absence, and Deletion against benchmark and flat-file baselines.
- LongMemEval-V2 across all five memory abilities and latency frontiers.
- EnterpriseRAG-Bench direct-floor, deterministic, enrichment, graph, temporal,
  and context-packing ablations.
- ACL-before-fanout, per-hop authorization, stale-deny, conversation isolation,
  deletion, bitemporal, tiny-budget, and partial-federation tests.
- Continual ingestion duplicate, crash, cursor, out-of-order deletion, extractor
  upgrade, partial multimodal, and projection rebuild tests.
