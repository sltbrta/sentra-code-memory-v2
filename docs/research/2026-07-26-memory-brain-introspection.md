<!-- markdownlint-disable MD013 MD024 MD004 MD029 MD032 MD036 -->

# Introspection: is Ouroboros a memory / company brain?

**Date:** 2026-07-27  
**Method:** Code/spec inventory + competitor/OSS survey + paper landscape  
**Companions:** [ARCHITECTURE.md](../../ARCHITECTURE.md) ·
[memory/README.md](../../services/brain/internal/memory/README.md) ·
[DEFERRED-AND-NON-GOALS](../roadmap/DEFERRED-AND-NON-GOALS.md) ·
[REMAINING-GAPS](../roadmap/REMAINING-GAPS.md) (walkable remaining inventory) ·
[SYNTHESIS.md](SYNTHESIS.md) · [PAPER-LANDSCAPE.md](PAPER-LANDSCAPE.md)

This is an **honest** assessment: what we are, what we are not, what human-memory
and competitor systems do that we lack, and what to adopt **natively** (not wholesale).

---

## 1. Bottom line

| Question | Answer |
| --- | --- |
| Is Ouroboros *akin* to a company brain / memory system? | **Yes, residual path** — sole `product-brain`; hosted IR + **memory cortex** + async gardener + authority substrate. |
| Is it a human-like memory system? | **No.** Multi-system human memory is the metaphor, not the claim. We ship evidence-grounded residual RAG + closed cortex loops, not biological sleep. |
| What closed (2026-07-27)? | Bi-temporal claims, utility→rank, C1 gate, episodes, PPR, PageIndex TOC, global PR *prior*, RAPTOR/community, agent STM/MTM/LTM, light ingest + async `RunCortexMaintenance` — see REMAINING-GAPS §1. |
| Still open / non-goal | LLM REM, LLM PageIndex walk, SCM session product, OpenIE density, LongMemEval SOTA, dual-plane ACL merge — REMAINING-GAPS §2–§5. |

We are a **local-first evidence-grounded company residual brain under one binary**,
not Mem0/Zep as the product, and not Lattice SMF desktop. Overclaiming “human
memory” remains wrong; underclaiming residual cortex as “just a job queue” is
also wrong after the closed loops.

---

## 2. What we actually have (introspection of shipped surface)

### 2.1 Systems we can honestly claim

```text
┌─────────────────────────────────────────────────────────────┐
│ product-brain (sole binary)                                 │
├──────────────┬──────────────────┬───────────────────────────┤
│ Residual IR  │ Code operator    │ Authority process         │
│ hosted       │ codecrawl/serve  │ ACL Git vault factory…    │
│ continual    │ codeindex P5     │ peer Unix RPC (live)      │
│ gardener     │                  │                           │
│ memory cortex│                  │                           │
│ productsec   │                  │                           │
│ tenant MVP   │                  │ federation local MVP      │
└──────────────┴──────────────────┴───────────────────────────┘
```

| Layer | Shipped substance | Memory analogue (loose) |
| --- | --- | --- |
| Ingest → chunks | Documents become generation-pinned passages | Encoding of *external* records |
| Light memory seed | Utility + doc texts + episode bind on hot path | Immediate episodic bind without heavy rewrite |
| Lexical/HotLex + dense + structure | Multi-arm retrieval + RRF + CE | Cued recall over corpus |
| Memory rank arms | Claim prefer, utility, global PR prior, PPR, pageindex, RAPTOR inject | Associative + hierarchical recall |
| Grounded answer | Claims cite passages; dual-cite on conflict | Source memory / reality monitoring |
| Continual watch | Delta ingest on doc change | Updating external store |
| Gardener enrich | doc2query, edges, dense sidecars (queue receipts OK) | Offline processing |
| Cortex maintenance | Extract, edges, PageIndex TOC, global PR, community | Heavy consolidation off hot path |
| Lifecycle | C1, NREM quarantine, det. REM, utility decay, RAPTOR, reseg | Sleep-phase API (deterministic, not LLM REM) |
| Agent memory | STM/MTM/LTM + promote; policy-gated | Working / intermediate agent notes |
| Conversation vault | Encrypted principal-scoped turns (authority) | Episodic *dialogue* store (narrow) |
| Code operator | Index/search/impact/route | Procedural/code *workspace* map — not skill learning |
| ACL / multi-principal / tenant | Who may recall what | Social/organizational memory boundary |
| Factory/tracer | Change runs + receipts | Procedural work log (company process) |

### 2.2 What we do **not** have (despite ambition or naming)

| Name / ambition | Reality check |
| --- | --- |
| Lattice **LLM** REM / sleep cycle | Deterministic REM re-extract only; `OUROBOROS_BRAIN_REM_LLM` is flag scaffold (NG-REM-LLM) |
| LLM PageIndex tree walk | Deterministic TOC + token match only (GAP-MEM-PAGEINDEX-LLM) |
| “Session” as SCM continuation | Chat turns / sealed product frames ≠ SCM agent session product (deferred) |
| Full Graphiti/Neo4j stack | Local JSON projection; not Neo product (non-goal) |
| Authority QueryService ≡ residual ask | Dual plane by design (GAP-PLANE-ASK) |
| Federation hive mesh | Local multi-brain MVP only |
| LongMemEval / memory SOTA | ERB primary; agent-memory suites not product gate |

---

## 3. Human memory / consolidation lens

Classic systems (Atkinson–Shiffrin, Baddeley working memory, Tulving episodic vs
semantic, hippocampal indexing, complementary learning systems / sleep
consolidation, Ebbinghaus forgetting, reconsolidation):

| Human factor | What it does | Ouroboros today | Gap severity |
| --- | --- | --- | --- |
| **Encoding specificity** | Memory is cue-dependent | Multi-query + doc2query + PageIndex section arm | Medium |
| **Episodic memory** | Bound events in time/place/self | Episodes bind + lifecycle reseg; company-life auto-segment thin | Medium (was High) |
| **Semantic memory** | Schemas, concepts, facts independent of episode | Bi-temporal claims + supersession + extract | Medium (was High) — denser OpenIE still partial |
| **Procedural memory** | Skills, habits | Factory/tracer for *changes*, not agent skill learning | High for agent product; medium for company brain |
| **Working memory** | Limited active buffer | Agent STM/MTM/LTM tiers (opt-in); no full MemGPT OS | Medium for agents (was High); SCM session still deferred |
| **Consolidation (NREM)** | Stabilize / transfer | NREM low-utility quarantine on cortex (no chunk delete) | Medium — closed MVP |
| **Consolidation (REM)** | Recombine / abstract | Deterministic re-extract; **not** LLM REM | Medium if det. enough; Critical only if claiming human-like LLM sleep |
| **Systems consolidation** | Hippocampus index → cortical schemas | Bipartite PPR multi-hop + RAPTOR/community | Medium — closed HippoRAG-style arm |
| **Forgetting / utility** | Adaptive pruning + decay | Half-life + cite reinforce **→ ranking**; NREM quarantine | Medium — **closed loop** |
| **Interference / conflict** | Overwrite or block old | Contest + ordered resolve + dual-cite abstain | Medium — multi-clique still open |
| **Reconsolidation** | Retrieval makes memory labile | Cite reinforce on retrieve; no free rewrite | Medium |
| **Metamemory / confidence** | Know what you don’t know | False-abstention / coverage retries + dual-cite | Medium — good start |
| **Emotional / salience tagging** | Priority encoding | Absent | Medium for company (incidents) |
| **Social / shared memory** | Common ground | ACL + federation cards | Medium |
| **Predictive coding / error-driven update** | Update when prediction fails | C1 hold-out probes gate heavy work | Medium — probes still thin vs real query logs |

**Verdict:** Residual path is a **self-updating fact projection + multi-arm IR +
async consolidation** under one binary. It is **not** a biological sleep system
or agent session OS. For remaining gaps, walk [REMAINING-GAPS.md](../roadmap/REMAINING-GAPS.md).

---

## 4. Competitor / product deep dive

### 4.1 Agent memory layers

| System | Core idea | Strength vs us | Weakness vs us | Adopt? |
| --- | --- | --- | --- | --- |
| **Mem0** ([arxiv:2504.19413](https://arxiv.org/abs/2504.19413), github.com/mem0ai/mem0) | Extract facts from conversation; user/session/agent memory; hybrid vector+graph+KV | Automatic memory write path from *dialogue*; conflict resolution; simple agent API | Weak company evidence authority; not Git/ACL/factory | **Adopt pattern:** extract→consolidate→retrieve API for *conversation*, not as sole brain |
| **Zep / Graphiti** ([arxiv:2501.13956](https://arxiv.org/abs/2501.13956), github.com/getzep/graphiti) | Bi-temporal knowledge graph; episodes → entities/edges with valid_from/to; invalidate without delete | True temporal fact model; incremental graph update; enterprise narrative | Not local-first vault; less code/factory | **Adopted pattern (residual):** bi-temporal claims + episode bind; not Neo4j wholesale |
| **Letta / MemGPT** | Hierarchical core/archival memory; OS-like paging; tools to self-edit memory | Working-memory discipline; agent-controlled store | Different trust model (agent writes memory) | **Partial:** STM/MTM/LTM + policy gate; full OS tools still open |
| **Supermemory / MemoryOS / Hindsight** | Consumer/agent memory OS tiers | Product UX for personal memory | Thin org security | Patterns only |
| **Claude Code / Hermes / CowAgent memory** | File-based MEMORY.md, daily notes, skill self-improve, FTS session search | Practical agent continuity | Not company ACL brain | Informs **SCM session product** (deferred) |

### 4.2 Retrieval / “RAG as memory”

| System | Core idea | Strength vs us | Adopt? |
| --- | --- | --- | --- |
| **HippoRAG / HippoRAG 2** ([arxiv:2405.14831](https://arxiv.org/abs/2405.14831), [2502.14802](https://arxiv.org/abs/2502.14802)) | Hippocampal indexing: OpenIE KG + Personalized PageRank multi-hop | Associative multi-hop without multi-round agentic search | **CLOSED (MVP):** bipartite PPR over phrase/doc graph; full index-time OpenIE still partial |
| **Microsoft GraphRAG** | Community summaries + map-reduce | Global sense-making over large corpora | **Partial:** community summaries in cortex; not full MS map-reduce pipeline |
| **HopRAG / RAPTOR** | Hierarchical clustering / hop | Long-doc abstraction | **CLOSED (MVP):** hierarchical + community RAPTOR inject post-NREM / cortex |
| **PageIndex (VectifyAI)** | Hierarchical TOC + optional LLM walk | Vectorless long-doc navigation | **CLOSED:** native deterministic TOC arm; **LLM walk open** |
| **Adaptive-RAG** | Route compute by difficulty | Cost control | Align with our prod vs quality profiles |

### 4.3 Company knowledge / SMF lineage (our own donors)

| System | Strength | Gap we still have vs best of them |
| --- | --- | --- |
| **Lattice / SMF** | Full lifecycle gardener, ontology packs, local-first, citations | Residual has C1/NREM/det.REM/utility→rank/cortex; still missing LLM REM, persona, smart-folder UI, continuous multi-week sleep product packaging |
| **SFS / SFS-Next** | Evidence, receipts, encryption contracts | Runtime answer path we lead; they lead pure contract rigor for multi-tenant objects |
| **Sentra Omni** | Multi-lane, extract cascade, federation ideas | Multi-tenant crypto platform depth; multi-host federation |
| **SCM** | Code operator verbs | Session product deferred |

### 4.4 Enterprise search / RAG vendors (pattern level)

Glean, Sourcegraph Cody, Notion AI, etc.: strong **connectors + ranking + UX**,
weak **local-first vault + fail-closed ACL + factory coupling**. We should steal
**ranking polish and connector breadth** later (DEF-004), not their cloud-only
trust model.

---

## 5. Research paper clusters (adoption-relevant)

| Cluster | Papers / anchors | Takeaway for Ouroboros |
| --- | --- | --- |
| Hippocampal RAG | HippoRAG, HippoRAG 2 | Dual-store (index + cortex-like passages); PPR multi-hop — **MVP closed** |
| Temporal graphs | Zep/Graphiti | Bi-temporal edges; episode nodes; supersession not delete — **MVP closed** |
| Agent memory services | Mem0 | Extraction pipeline + LOCOMO-style eval — eval still open |
| Memory OS | MemoryOS (EMNLP 2025), MemGPT | STM/MTM/LTM hierarchy with explicit promotion — **tiers closed**; OS tools partial |
| Consolidation / fragility | Nemori; “Useful Memories Become Faulty” | Predict-calibrate before rewrite — **C1 MVP closed** |
| Sleep / utility | LightMem, SCM arxiv lines Lattice cites | NREM/REM split; utility decay with **retrieval reinforcement** — det. path closed; LLM REM non-goal |
| Benchmarks | LongMemEval, MEME, EnterpriseRAG, LOCOMO, DMR | We hold ERB; need LongMem/agent-memory suite if we claim memory SOTA |
| Authz | Zanzibar | We already align filter-before-fanout; keep |
| Harness | AHE, OpenAI/Anthropic harness posts | Memory changes need holdout promotion too |

Full paper index remains [SOURCES.yaml](SOURCES.yaml) / [PAPER-LANDSCAPE.md](PAPER-LANDSCAPE.md).

---

## 6. Deep gap register (CLOSED vs still open)

Authoritative walkthrough of **remaining** work:
**[REMAINING-GAPS.md](../roadmap/REMAINING-GAPS.md)**. Below is the research-era
register with status stamps.

### CLOSED — residual company-brain program (do not re-open as missing)

| # | Theme | Shipped |
| --- | --- | --- |
| 1 | Bi-temporal claims | valid_from/to, observed_at, supersede, contested |
| 2 | Episodes | bind, reseg CLI, lifecycle reseg, episode filter env |
| 3 | Utility → ranking | half-life + cite reinforce closed loop |
| 4 | C1 predict-calibrate | hold-out probes; skip heavy when healthy |
| 5 | Conflict / dual-cite | ordered resolve + dual_cite_and_abstain |
| 6 | HippoRAG-style PPR | doc co-occur + bipartite phrase seeds |
| 7 | RAPTOR / community | lifecycle / cortex inject |
| 8 | PageIndex TOC | native Go deterministic trees; channel `pageindex` |
| 9 | Global PageRank prior | offline mild boost only (not primary ranker) |
| 10 | Heavy cortex off ingest | light seed; `RunCortexMaintenance` post-wave |
| 11 | Agent STM/MTM/LTM | policy-gated put/get/search; opt-in inject |
| 12 | Deterministic REM | re-extract + low-conf quarantine (not LLM) |

### Still open / partial / non-goal (Tier A–C)

| Priority | Item | Status |
| --- | --- | --- |
| A | LLM REM re-encode | `[non-goal]` NG-REM-LLM |
| A | LLM PageIndex tree walk | `[open]` GAP-MEM-PAGEINDEX-LLM |
| A | Continuous multi-week sleep daemon packaging | `[partial]` |
| A | OpenIE / dense SPO extract | `[partial]` |
| A | Multi-clique conflict | `[open]` |
| A | Rich ontology packs | `[partial]` |
| A | GraphRAG full map-reduce | `[partial]` |
| B | SCM session continuation | `[non-goal]` NG-SCM-010 |
| B | Full MemoryOS/MemGPT tools | `[partial]` |
| C | Connectors, clients, HA, OpenFGA live, multi-host federation | see REMAINING-GAPS §4 |

### Explicitly do **not** adopt wholesale

| Trap | Why |
| --- | --- |
| Mem0 as the product brain | Loses evidence authority / Git / factory |
| Agent free-write memory | Breaks ACL and provenance |
| Full reindex every change | SCM reject; we already do stamp warm |
| Unbounded REM spend | Latency / cost / over-consolidation |
| Claiming SOTA without LongMem/DMR | Honesty |
| Merging authority QueryService into residual ask | Dual-plane by design |

---

## 7. Are we a “memory/computer brain”? Scorecard

| Dimension (0–5) | Score | Note |
| --- | --- | --- |
| External knowledge store + retrieval | **4** | Residual IR + PageIndex section arm + multi-arm |
| Provenance / grounding | **4** | Differentiator vs Mem0-class |
| Organizational security | **3.5** | Authority strong; residual multi-tenant MVP |
| Code workspace memory | **3** | Operator yes; session no |
| Temporal / evolving world model | **3.5** | Claims + dual-axis + contest/supersede; extract off hot path |
| Consolidation that improves future recall | **3.5** | Async cortex maintenance + NREM/REM/utility/community RAPTOR |
| Episodic company memory | **3** | Episode bind + lifecycle reseg; not full company-life segmentation |
| Working memory for agents | **2** | STM/MTM/LTM tiers + promote; **SCM session still deferred** |
| Forgetting / interference control | **3** | Half-life + NREM/REM quarantine + multi-valued policy |
| Overall “company brain” | **~3.5** | One residual cortex under product-brain; not agent session product |

**Interpretation:** Residual company brain is **one path** (`hosted` + `memory` +
`gardener` under sole `product-brain`). Heavy cortex (extract, edges, PageIndex,
global PR prior, community summaries) runs **async post-wave**, not on ingest.
Global PageRank is a **weak prior only**; multi-arm IR remains primary. PageIndex
is native deterministic TOC (VectifyAI concepts, no Python). **Still open /
deferred:** full LLM PageIndex tree walk, LLM REM re-encode, SCM session
continuation packets, LongMemEval SOTA claims, merging authority QueryService
into residual ask.

*Scorecard refreshed 2026-07-27 after async cortex + PageIndex + global PR + agent tiers.*

---

## 8. Recommended adoption sequence (if we choose to deepen “memory”)

```text
CLOSED path (already on main — do not re-plan as greenfield):
  bi-temporal claims · episodes · utility→rank · C1 · PPR · RAPTOR
  · PageIndex TOC · global PR prior · agent tiers · det. REM

Next leverage (only if prioritized — see REMAINING-GAPS):
  1. LLM PageIndex walk (budgeted) OR denser OpenIE extract
  2. Real query-log C1 probes + continuous sleep packaging
  3. Multi-clique conflict + evidence spans
  4. Optional: SCM session product as separate track
```

Do **not** start with more job kind names. Prefer **representations and rank
arms that change what gets recalled tomorrow**, with honest non-goals for LLM
REM and second brands.

---

## 9. Sources (non-exhaustive)

- Mem0: arxiv 2504.19413; github.com/mem0ai/mem0  
- Zep/Graphiti: arxiv 2501.13956; github.com/getzep/graphiti  
- HippoRAG / 2: arxiv 2405.14831, 2502.14802; github.com/osu-nlp-group/hipporag  
- MemoryOS: arxiv 2506.06326 / EMNLP 2025; github.com/BAI-LAB/MemoryOS  
- PageIndex concepts: github.com/VectifyAI/PageIndex (native Go reimplementation)  
- In-repo: SYNTHESIS.md, PAPER-LANDSCAPE.md, LOCAL-ARCHAEOLOGY.md, Lattice GARDENER.md  
- Product truth: ARCHITECTURE.md, memory/README.md, gardener/doc.go,
  DEFERRED-AND-NON-GOALS.md, REMAINING-GAPS.md, phase 01–05 specs

---

*Refreshed 2026-07-27: async cortex maintenance, native PageIndex, global PR prior,
bipartite PPR seeds, agent STM/MTM/LTM; P2 SCM session + LLM tree walk deferred;
no LongMemEval SOTA claim; §1–§3 and §6 distinguish CLOSED vs remaining.*
