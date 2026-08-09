# memory

Cohesive **product-brain memory cortex** (not a second binary). Projection-only:
`<brain>/memory/memory.json` never mutates primary evidence digests.

## Module table

| Module | File(s) | Role | Loop status |
| --- | --- | --- | --- |
| claims | `claims.go` | Bi-temporal facts + supersession + ExpiredAt (SFS/Graphiti) | **closed** |
| relations | `relations.go` | Bi-temporal entity edges + supersede/contest + ExpandRelations | **closed** |
| extract | `extract.go` + `extract_llm.go` | Det denser OpenIE (ops + **general product** graph: depends_on/integrates_with/owned_by/…) + opt `OPENIE_LLM` + span fill | **closed** (gardener cortex) |
| conflict | `conflict.go` | Ordered resolver ladder; multi-valued preds skip contest | **closed** |
| ontology packs | `ontology/policy.go` + `packs/default.yaml` | Multi-valued predicates (`tags`, `aliases`, …) | **closed** (P1) |
| episodes | `episodes.go` | Time-bounded evidence bindings + lifecycle reseg | **closed** |
| utility | `utility.go` | Half-life decay + cite reinforcement → ranking | **closed** |
| quarantine | `quarantine.go` | NREM low-utility quarantine (no chunk delete) | **closed** |
| rem | `rem.go` | Opt-in deterministic REM re-extract + low-conf quarantine | **closed** (no LLM) |
| edges | `edges.go` | Weighted doc edges + claim-link + C5 hyp + prune | **closed** |
| expand | `expand.go` | Bounded BFS claim/doc expand (`ExpandFromSeeds`) | **closed** (opt-in diag) |
| c1 | `c1.go` + `querylog.go` | Hold-out probes; prefer gold DocIDs from query_log | **closed** |
| ppr | `ppr.go` | Personalized PageRank multi-hop (HippoRAG `phrase:` seeds) | **closed** |
| pagerank | `pagerank.go` | **Global** PageRank prior (offline; mild boost) | **closed** |
| pageindex | `pageindex.go` + `pageindex_llm.go` | TOC trees + opt LLM walk on **user question** | **closed** |
| graphrag | `graphrag.go` | Community map-reduce + opt `GRAPHRAG_LLM` reduce | **closed** |
| cortex | `cortex.go` | `RunCortexMaintenance` heavy offline build | **closed** |
| raptor | `raptor.go` | Hierarchical + community summaries | **closed** (post-NREM / cortex) |
| agentmem | `agentmem.go` | Policy-gated agent notes with STM/MTM/LTM tiers | **closed** (opt-in inject) |
| store | `store.go` | Durable JSON projection | **closed** |

## Design decisions

### Left-shift: structure encodes intelligence

Heavy intelligence is computed at **gardener / lifecycle** time. Claims project
to Graphiti-class **TemporalRelations** via `SeedRelationsFromClaims` so lean
serve only walks precomputed edges (`ExpandRelations`) — no OpenIE at ask.

### Full-corpus PageRank is a weak prior only

Lean serve (HotLex + dense + structure SQL) remains **primary**. Global PageRank
is computed offline over the doc co-occur graph during cortex maintenance and
applied as a mild multiplicative boost after utility
(`score *= 1 + 0.15 * pr_norm`). Not the primary ranker. Diag: `global_pr:
true`.

### PageIndex (native, vectorless section arm)

Inspired by [VectifyAI/PageIndex](https://github.com/VectifyAI/PageIndex) (MIT
concepts), **reimplemented in Go** — no Python import. Deterministic
hierarchical
TOC per document (`#`/`##`, Title Case lines, numbered sections, paragraph
clusters). Retrieve: token-match query against node titles/summaries → inject
leaf section text as passages channel `pageindex`. **Optional LLM tree walk**
when `OUROBOROS_BRAIN_PAGEINDEX_LLM=1|true|yes` (chooser menu includes title +
summary; walk uses the real user question).

### Heavy work off ingest hot path

`seedMemoryAfterIngest` is **LIGHT**: `EnsureUtility` + `SetDocTexts` +
`BindEpisode` only.

Heavy cortex (`RunCortexMaintenance`): det/LLM OpenIE →
**SeedRelationsFromClaims**
→ prose co-occur + `phrase:` HippoRAG seeds → PageIndex → global PageRank →
community/RAPTOR/GraphRAG — called from `RunGardenerWave` (post-wave) and
`gardener --lifecycle`. REM re-extract also reseeds TemporalRelations.

## Closed loops

```text
ingest (hot): EnsureUtility + SetDocTexts + BindEpisode

gardener wave / --lifecycle:
  RunCortexMaintenance
    → extract claims → SeedRelationsFromClaims (TemporalRelations)
    → edges + pageindex + global PR + community
  → [lifecycle] C1 → NREM → [opt] REM (+ reseed relations) → RAPTOR → reseg

LEAN retrieve (default):
  HotLex + dense → RRF → structure hop (path2 SQL / ExpandRelations)
  → CE → claim prefer · utility · PPR · pageindex · RAPTOR inject
  → dual-cite on contested · cite reinforce utility

QUALITY / research (opt-in): multi-arm residual + HyDE/phrase + agentic
```

## Env / CLI

| Knob | Effect |
| --- | --- |
| `OUROBOROS_BRAIN_AS_OF` / `KNOWN_AT` | Dual-axis claim preference (RFC3339) |
| `OUROBOROS_BRAIN_PPR=0` | Disable PPR multi-hop |
| `OUROBOROS_BRAIN_GLOBAL_PR=0` | Disable global PageRank prior |
| `OUROBOROS_BRAIN_EPISODE_ID` | Filter retrieve to episode docs |
| `OUROBOROS_BRAIN_REM=1` / `--rem` | Deterministic REM after NREM |
| `OUROBOROS_BRAIN_REM_LLM=1` | Flag LLM extension only (no network) |
| `OUROBOROS_BRAIN_AGENT_MEM=1` + `OUROBOROS_BRAIN_PRINCIPAL` | Inject agent notes (tier-ordered) |
| `OUROBOROS_BRAIN_GARDENER_AUTO=1` | Background enrich+cortex after OpenLocal |

CLI: `product-brain memory …`, `ask --as-of/--known-at`,
`gardener --lifecycle [--rem] [--lifecycle-interval 30m]`.

## Non-goals (this package)

- No second binary / Mem0-Zep wholesale import
- No importing PageIndex Python
- No LLM-required REM / PageIndex tree walk (extension points only)
- No SCM session continuation product (P2 deferred)
- No LongMemEval SOTA claims
- No merging authority QueryService into residual ask
