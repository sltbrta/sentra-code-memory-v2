<!-- markdownlint-disable MD013 MD022 MD032 MD058 -->

# Structure-brain foundation — research synthesis + stage gates

**Branch:** `feat/structure-brain-foundation`  
**Thesis:** Structure encodes intelligence. The **computed brain** (claims, bi-temporal edges, conflict, episodes, gardener lifecycle) is stage 0. Query polish without structure is a cold chunk shell.

**Non-goal this phase:** Full-500 ERB leaderboard runs. Subset / unit / adversarial only until every stage gate is green.

---

## 1. What already exists (earn-its-keep audit)

| Capability | Location | Status | Earns keep? |
| --- | --- | --- | --- |
| Bi-temporal **claims** (valid_from/to + observed_at) | `memory/claims.go` | **shipped**, tests green | **Yes** — Graphiti/Zep core |
| Supersede / superseded_by | `AdmitClaim`, `SupersedeClaim` | **shipped** | **Yes** |
| Conflict ladder (multi-valued, supersession, quality, docs, window, tie→contested) | `memory/conflict.go` | **shipped** | **Yes** — never silent UUID |
| Contested dual-cite + abstain on ask | `hosted/memory_cortex.go` | **shipped** | **Yes** — ERB conflicting_info |
| Async gardener (d2q, edge_propose, context) | `gardener/` | **shipped** | **Yes** — left-shift |
| Lifecycle NREM / REM / utility / prune (dream) | `gardener/lifecycle.go` | **shipped** | **Yes** — consolidation |
| Episodes, utility half-life, quarantine | `memory/` | **shipped** | **Yes** |
| PPR / RAPTOR / GraphRAG / PageIndex | `memory/` | **shipped** | **Yes** if cortex attached |
| Path2 entities/facts/rels (~2M/4.6M/4.6M on full-bench-v2) | Neon SMF | **data live** | **Consumed on serve** — SMF schema SQL + detached structure budget |
| Product co-occur edges (doc↔doc only, no bi-temporal) | `memory/edges.go`, product SQL | partial | TemporalRelations + path2 facts cover multi-hop; residual co-occur stays light |
| Query path on path2 full-bench | HotLex/dense + path2 structure SQL | **active** | Structure arm + window/cite; subset hit-rate live |

**Verdict:** Do **not** rewrite cortex. **Close the consume gap** and add **Graphiti-class bi-temporal relations** where claims alone are not enough for multi-hop graph walks.

---

## 2. Research: papers, competitors, OSS (≥20)

### Papers / platforms
| Source | Load-bearing idea | Adopt / skip |
| --- | --- | --- |
| Zep arXiv 2501.13956 | Bi-temporal KG: world time T + transaction T′ | **Adopt** (claims already; extend relations) |
| Graphiti (getzep/graphiti) | Fact edges with valid_at/invalid_at; non-lossy history; conflict search | **Adopt model**, reimplement natively |
| Microsoft GraphRAG | Community map-reduce | **Have** (`graphrag.go`) |
| RAPTOR | Hierarchical summaries | **Have** (`raptor.go`) |
| HippoRAG | Phrase↔doc bipartite + PPR | **Have** (`ppr.go`, phrase edges) |
| LightRAG / nano-graphrag | Lightweight entity graphs | Overlap; skip second stack |
| CRAG | Corrective retrieval grade | Partial (agentic grade) |
| Self-RAG | Critique tokens | Skip — cite ground is enough |

### Competitors / agent memory

| System | Idea | Earn keep? |
| --- | --- | --- |
| **Zep/Graphiti** | Bi-temporal fact graph, episodic nodes | **Yes** — reference model |
| **Mem0** | Vector + entity hints | Thin temporal — skip as primary |
| **Letta** | OS-style memory blocks | Different product — skip import |
| **Cognee** | Extract–cognify–load KG | Overlaps cortex extract — no second pipeline |
| **LangGraph / LlamaIndex KG** | Orchestration | Substrate only |

### OSS repos (concepts reimplemented or already present; no wholesale import)

1. getzep/graphiti — bi-temporal edges  
2. microsoft/graphrag — communities  
3. OSU-NLP-Group/HippoRAG — PPR  
4. parthsarthi03/raptor — hierarchy  
5. HKUDS/LightRAG — entity graph  
6. VectifyAI/PageIndex — vectorless TOC (**native Go**)  
7. topoteretes/cognee — extract pipeline  
8. mem0ai/mem0 — memory API shapes  
9. letta-ai/letta — tiered memory  
10. langchain-ai/langgraph — tool loop  
11. run-llama/llama_index — KG query engines  
12. neo4j/graph-data-science — graph algos inspiration  
13. falkordb/falkordb — graph store option later  
14. perplexity-ai/OpenIE / dygiepp-class extract — OpenIE denser  
15. stanford-futuredata/ColBERT — late interaction (ablation only)  
16. facebookresearch/faiss — ANN (we have Qdrant/HNSW)  
17. qdrant/qdrant — dense ANN  
18. weaviate/weaviate — hybrid inspiration  
19. milvus-io/milvus — scale patterns  
20. chroma-core/chroma — simplicity (not primary)  
21. nomic-ai/gpt4all / local embed — BYOC  
22. OpenSPG/KAG — knowledge-augmented gen patterns  

---

## 3. Stage plan (serial; no advance until gate green)

| Stage | Name | Gate (must pass) |
| --- | --- | --- |
| **S0** | Cortex foundation | All `memory` + `gardener` unit tests green; bi-temporal + supersede + conflict + dual-axis knownAt tests; lifecycle plan deterministic |
| **S1** | Temporal relations | Bi-temporal **claim relations** (entity–predicate–entity edges) with valid/observed/expire; supersede on edges; unit tests |
| **S2** | Brain build left-shift | Product ingest writes structure; path2 **consumes** SMF entities/facts/rels on serve; `ProbeBrainStructure` OK |
| **S3** | Gardener/dream attach | Async enrich + lifecycle always on product OpenLocal; path2 optional cortex attach when `BRAIN_DIR` set |
| **S4** | Retrieve consumes structure | Subset 40Q: structure arm hit rate; pool_recall diags; no full-500 |
| **S5** | Window / cite / synth | Only after S4 |
| **S6** | Integration + adversarial | Full pipeline mock + subset; then full-500 |

**Remove if not earning keep:** duplicate co-occur indexes that never feed retrieve; QUALITY-only path2 structure gate; FORCE_PATH2_FTS disabling HotLex.

---

## 4. Pipeline reshape (left-shift / lean query)

```text
BUILD / BURST / GARDENER (heavy, async OK)
  ingest chunks → product structure co-occur
  cortex: bi-temporal claims + TemporalRelations
  HotLex project · dense warm · d2q/context sidecars
  lifecycle: NREM/REM · utility · RAPTOR · C1

LEAN QUERY (everyday + ERB serve)
  HotLex BM25 (if gob) + dense ANN  [parallel]
  RRF → sibling hydrate
  structure: pool-virtual + path2 SQL read (precomputed) + ExpandRelations
  CE (lexical interactive / ZE when quality)
  coverage window → synth → ground → conflict dual-cite
```

**Removed / demoted:** QUALITY-only path2 structure gate; FORCE_PATH2_FTS disabling HotLex;
default unbounded multi-arm as the everyday ask path (QUALITY opt-in only);
query-time OpenIE (left-shifted to gardener cortex + REM).

---

## 5. Pipeline delta (adds / removes / demotes)

### Added

| Surface | What |
| --- | --- |
| `memory.TemporalRelation` | Bi-temporal entity edges: valid/observed/expire, supersede, contest, ExpandRelations |
| `Claim.ExpiredAt` + KnownAt dual-axis | Complete Graphiti-class claim timelines |
| `SeedRelationsFromClaims` / `RelationFromClaim` | Cortex + REM left-shift: extract → graph (no extract at ask) |
| `ExpandRelationDocuments` + `temporalRelationPassages` | Serve promotes **document IDs** from TemporalRelations (not bare entity names) |
| Path2 structure on **serve** | Budgeted SQL read of SMF entities/facts/rels always-on (not QUALITY-only) |
| Structure hop on lean + residual | HotLex + residual path2: pool-virtual + path2 SQL + cortex docs |
| HotLex fused into QUALITY | Residual multi-arm still includes HotLex when projected |
| Docs | ARCHITECTURE §3.2 left-shift, brain/memory READMEs, GLOSSARY, root README |

### Removed / demoted

| Change | Why |
| --- | --- |
| QUALITY-only path2 structure gate | Structure tables were dead on default serve |
| FORCE_PATH2_FTS killing HotLex | Prefer HotLex for lean interactive serve |
| Default unbounded multi-arm as “the” ask path | Demoted to QUALITY/research opt-in |
| Query-time OpenIE / re-extract | Left-shifted to gardener cortex + REM |

### Retained (explicit)

- FS projection (`CreateLocal` / `OpenLocal`) — agent-friendly dir layout
- One product path (`hosted.Client`) — ERB is a score surface, not a second pipeline
- Bi-temporal claims, conflict ladder, dual-cite/abstain, gardener lifecycle

---

## 6. Stage status (this branch)

| Stage | Status |
| --- | --- |
| **S0** Cortex foundation | **landed** — memory + gardener unit gates |
| **S1** Temporal relations | **landed** — relations_test + SeedRelationsFromClaims |
| **S2** Brain build left-shift | **landed** (unit) — FS build + ExpandRelationDocuments on serve/residual |
| **S3** Gardener/dream attach | **landed** (unit) — sync enrich + **async** `RunGardenerWave` / daemon post-wave cortex: extract → SeedRelationsFromClaims; REM reseeds relations; lifecycle CLI reports `relations` / `rem_relations` |
| **S4** Path2 structure subset | **landed** — SMF schema align; detached structure SQL budget on retrieve/interactive (no parent-starve); live expand + retrieve hard-gate `path2_sql` + `structure_sql_promoted≥1` |
| **S5** Window / cite / synth | **landed** (unit+live subset) — structure MMR prior + soft id floor; ground cites; general product OpenIE (depends_on/integrates_with/…); live subset structure arm hit-rate ≥0.60 |
| **S6** Integration + adversarial | **partial** (unit) — `TestS6AdversarialProductPathIntegration` full FS path: structure ask, dual-cite, info_not_found cap, illegal cite strip, HotLex/QUALITY split; **full-500 still deferred** |

---

## 7. Confidence gates

| Gate | Evidence |
| --- | --- |
| `go test ./services/brain/internal/memory/` | bi-temporal claims, relations, supersede, expire, seed-from-claims |
| `go test ./services/brain/internal/gardener/` | async plan + lifecycle |
| `go test ./services/brain/internal/hosted/ -run BrainBuild` | FS product ingest → cortex → lean ask |
| `TestAsyncGardenerWaveLeftShiftsTemporalRelations` | default async enqueue → wave → TemporalRelations + lean `temporal_relation_docs` |
| `TestSyncEnrichLeftShiftsTemporalRelations` | ENRICH=sync post-wave cortex without gardener CLI |
| `TestPreferInteractiveWhenHotLexPresent` | HotLex wins over FORCE_PATH2_FTS / QUALITY |
| Path2 structure on serve | `retrieve.go` always reads path2 SQL (budgeted) |
| `TestPath2StructureLiveExpand` | Neon full-bench-v2: structure docs≥1, retrieve `path2_sql` + sql_promoted (skip without secrets) |
| `TestStructureChannelSurvivesWindowAndGround` | S5 unit: path2_structure survives coverage+window and ground cite |
| `TestOpenIEGeneralProductGraph` / `TestLeanServeGeneralProductTemporalRelations` | general product graph extract → TemporalRelations → lean serve |
| `TestPath2StructureLiveSubsetHitRate` | Neon: ≥60% probes path2_sql + structure_sql_promoted≥1 |
| `TestS6AdversarialProductPathIntegration` | FS E2E adversarial: structure + contest + abstain cap + ground strip |

**FS projection retained:** `CreateLocal` / `OpenLocal` dir layout unchanged (agent-friendly).

## 8. Non-claims

- No SOTA / leaderboard score from this phase.
- No second memory product brand.
- Graphiti/Zep concepts only — reimplemented under product-brain.
- Full-500 deferred until explicitly authorized (S6 unit adversarial is not a leaderboard run).
