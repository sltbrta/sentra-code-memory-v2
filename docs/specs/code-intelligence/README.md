# Code intelligence and transactional ChangeSet specification

Status: **[planned]**. This is the canonical code-intelligence domain
specification; no runtime change engine exists yet.

## 1. Objective

Ouroboros must produce accurate, coherent, properly integrated changes while
using the least context and model work that preserves those properties.

“Multi-edit by default” means one behaviorally coherent transaction across all
required files and artifacts. It does not mean making unrelated large changes
or trusting one giant model response.

## 2. Layered code intelligence

No single code graph is complete enough to own impact analysis. Use evidence
lanes with explicit authority and freshness:

1. **Snapshot and lexical lane**
   - Git trees/blobs, manifests, ownership, generated markers, exact/regex/FTS.
2. **Syntax lane**
   - tree-sitter definitions/imports/routes/config keys/tests and
     stack-graph-style independently stitchable file subgraphs.
3. **Precise semantic lane**
   - live LSP/compiler services against the candidate workspace;
   - SCIP indexes for committed and cross-repository definitions/references.
4. **Build and behavior lane**
   - package/build graph, API/schema generation, migrations, config bindings,
     tests/coverage, runtime traces, flags, and deployment topology.
5. **Company-brain lane**
   - requirements, decisions, incidents, owners, conversations, customers,
     policies, and evidence connected to code artifacts.

Compiler/SCIP evidence outranks syntax inference. Runtime observations augment
static structure but do not rewrite it. Agent-inferred edges remain proposals
until confirmed. Every edge records source lane, snapshot, tool/config version,
provenance, authority, confidence, time, ACL, and counterevidence.

Use [LSP
`WorkspaceEdit`](https://microsoft.github.io/language-server-protocol/specifications/lsp/3.18/specification/)
as an interoperability envelope, [SCIP](https://github.com/scip-code/scip) as
the compiler-accurate snapshot format, and tree-sitter/stack-graph algorithms
as broad-language fallbacks. GitHub's stack-graphs implementation is archived,
so it is an algorithmic donor rather than a strategic dependency.

## 3. Incremental indexing

Content-address each file's syntax subgraph and each compiler/index artifact.
A change invalidates the local subgraph and dependent stitched paths, not the
whole corpus by default. Add/change/delete/rename reconciliation is idempotent.

The candidate tree is indexed before validation. Periodic clean rebuilds are
compared to incremental state to detect drift. Missing/stale language coverage
lowers confidence and widens verification.

Source Context Memory/Cortex contracts and fixtures may be adapted, but its
current full reindex, TS/JS-only assumptions, approximate token accounting, and
heuristic signature checks are not the target architecture.

## 4. Canonical contracts

```ts
type CodeSnapshot = {
  repository: RepositoryRef;
  treeOid: GitOid;
  toolchainLockDigest: Digest;
  workspace: WorkspaceRef;
  indexRevisions: Record<CodeLane, ProjectionRevision>;
};

type ChangeSet = {
  id: ChangeSetId;
  base: CodeSnapshot;
  intent: ChangeIntentRef;
  decisionAndSpecLinks: ArtifactRef[];
  operations: ChangeOperation[];
  expectedVersions: ArtifactVersionRef[];
  writeLease: LeaseRef;
  predictedImpact: ImpactReceiptRef;
  preconditions: Condition[];
  postconditions: Condition[];
  provenance: ProducerProvenance;
};

type ChangeOperation =
  | CreateFile | DeleteFile | MoveFile | ExactTextEdit
  | LspWorkspaceEdit | CompilerRefactor | StructuralRewrite;

type ImpactReceipt = {
  changeSetDigest: Digest;
  graphWatermark: GraphWatermark;
  directlyChanged: ArtifactRef[];
  closure: ImpactPath[];
  obligations: ChangeObligation[];
  unknowns: CoverageGap[];
  risk: RiskClass;
};

type ValidationPlan = {
  minimumGates: VerificationGate[];
  selectedBy: EvidencePath[];
  widenedBecause: CoverageGap[];
};

type PromotionRecord = {
  baseTree: GitOid;
  candidateTree: GitOid;
  evidence: VerificationReceiptRef[];
  reviews: ReviewReceiptRef[];
  compareAndSwap: "succeeded" | "stale" | "rejected";
  rollbackParent: GitOid;
};
```

Durable edits use blob hashes and exact byte/source anchors. LSP UTF-16
positions, model search/replace blocks, unified diffs, and whole-file output are
runner adapter formats, not canonical identities.

## 5. Transaction flow

```text
freeze base Git tree + acquire fenced write scope
→ compute graph-backed impact and obligations
→ let the model use its certified native edit format
→ normalize into ChangeSet
→ validate hashes, paths, symlinks, ACLs, scopes, indexes, match counts
→ apply all operations in an isolated copy-on-write workspace
→ format and incrementally re-index the candidate
→ compare predicted and observed graph delta
→ add newly discovered obligations and widen gates
→ parse/typecheck/compile/test/security/docs/generated-output verification
→ independent fresh-eyes review
→ produce candidate Git tree/commit
→ compare-and-swap branch/ref after current authorization
→ emit events and rebuild projections
```

Failure discards the candidate and leaves the canonical tree untouched.
Concurrent modification returns a typed stale-base result and requires
reanalysis/rebase. Fuzzy matching may propose a corrected operation but never
silently selects a different target.

Known repeated transformations use compiler refactors, LSP, ast-grep, or
semantic patches. A model may propose the structural rule; deterministic
machinery enumerates exact matches, previews, applies, and verifies it.

## 6. Blast-radius closure

Compute impact for every `ChangeSet`, including docs-only changes. Seed from
changed files, symbols, signatures, schemas, config keys, requirements, and
decisions. Traverse typed relations including:

- calls, references, implements, inherits, imports;
- package, build, and deployment dependencies;
- contract/schema producer and consumer;
- generates/generated-from;
- verifies/tested-by/covered-by;
- describes/documented-by/example-of/docstring-of;
- reads-config/feature-flagged-by;
- owned-by/governed-by/decision-justifies/incident-implicates;
- cross-language and cross-repository bindings.

`affects` is a derived result with proof paths, never an opaque stored edge.
The closure creates explicit code, test, docs, contract, schema, config, infra,
review, owner, and deployment obligations.

Low confidence, dynamic dispatch, reflection, generated code, incomplete
compiler indexes, stale runtime traces, or unexpected candidate deltas expand
the verification set and remain visible to the reviewer.

## 7. Context and token efficiency

Tools/capabilities are lazily described and hydrated. A context packet contains:

- concise task-personalized repo map;
- exact relevant signatures/source slices;
- callers/callees, contracts, tests, docs, decisions, and owners;
- impact receipt and uncertainty;
- relevant authorized session/user history and brain evidence;
- current acceptance and verification minimums.

Rank with exact search, compiler symbols, graph proximity, task-personalized
PageRank, recency, and reciprocal-rank fusion. Hydrate full source only after
ranking. Preserve a direct-source floor.

Measure provider-reported input/output/cache tokens, schema tokens, retrieved
versus cited tokens, redundant context, context precision, impact recall, tool
calls before valid edit, patch versus whole-file bytes, index cost per task,
verified changes per million uncached tokens, and escaped defects.

## 8. Conformance suite

- Failure on the final operation of a ten-file batch leaves canonical source
  unchanged.
- Crash at every staging/promotion boundary yields the old or complete new tree,
  never a mixture.
- One stale target blob rejects the entire batch.
- Rename/edit ordering, duplicates, add/delete collisions, BOM/line endings,
  and formatter output are deterministic.
- Path traversal, symlink escape, case collision, out-of-scope/generated edits,
  and overlapping worker scopes fail closed.
- Incremental index equals clean rebuild across add/change/delete/rename.
- Candidate diagnostics use candidate state, never stale disk.
- Public signature changes reach callers, implementations, tests, docs,
  examples, schemas, config, and downstream packages.
- Dynamic/reflection fixtures lower confidence and broaden verification.
- Seeded graph false negatives are caught by candidate delta, compiler,
  integration, or widened gates.
- Rollback restores source plus matching graph/docs/status projections.

The benchmark matrix compares grep/read, syntax repo map, syntax graph,
compiler/LSP+SCIP, and the full company-linked graph under the same
model/runner/budget.
