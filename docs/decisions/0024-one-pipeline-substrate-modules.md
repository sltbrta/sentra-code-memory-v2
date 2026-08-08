<!-- markdownlint-disable MD013 MD024 MD029 MD032 -->

# ADR 0024 — One residual pipeline; per-module substrate choice

**Status:** Accepted; **MVP wiring shipped** (2026-07-27); CLI full-pipeline bind
(create|ingest|ask|gardener via `OpenResidual`) closed same day.  
**Date:** 2026-07-27  
**Context:** Local vs “hosted” was described as profiles/adapters; Sammy clarified the
intended model is **one process, one flow**, with **local vs remote only as
pluggable substrates per module**, not two product paths.

**MVP shipped surface:** `hosted.SubstrateConfig` + `ApplySubstrates` / `OpenResidual` /
`OpenMemoryWithSubstrates`; CLI `create|ingest|ask|gardener` share `OpenResidual`
with `--chunks`/`--backend`/`--queue`/`--cortex`/`--substrate-profile` and
`OUROBOROS_BRAIN_*` env; queue rebind honors overrides after OpenLocal; solo FS
and mixed (e.g. FS chunks + memory queue + FS cortex) proven on dual-cite +
utility ranking.

## Decision

1. **One process** — residual company brain runs in a single `product-brain`
   process (or one long-running service instance). No “local brain process” vs
   “hosted brain process” dual.

2. **One pipeline** — the control flow is always the same:

   ```text
   ingest/watch → retrieval_ready → enrich queue → cortex maintenance
                → retrieve multi-arm → ground / dual-cite / reinforce
   ```

   Profiles (prod/quality/interactive) adjust **budgets**, not the pipeline shape.

3. **No product divide named “local” vs “hosted.”**  
   Those words mean **where a module’s durable state lives**, not which brain
   you are running. Marketing and docs should prefer:

   - **pipeline** (fixed)
   - **substrate** (pluggable per module)

4. **Per-module substrate selection** — each durable concern has an interface;
   operators (or a deploy profile) pick an implementation:

   | Module (concern) | Role | Example substrates |
   | --- | --- | --- |
   | **Chunks / evidence projection** | retrieval_ready passages | FS jsonl · Neon product_* · (future) object store |
   | **Lexical serving index** | interactive BM25 | HotLex gob file · remote HotLex blob |
   | **Dense ANN** | semantic arm | none · **sqlite dense.db** · Qdrant · (roadmap) FAISS · pgvector |
   | **Structure graph** | edges/entities/facts | in-memory · Neon structure tables · SQLite |
   | **Gardener queue** | async jobs | **SQLite durable** · memory+Dir→sqlite · ephemeral tests · (roadmap) Postgres |
   | **Memory cortex** | claims, PageIndex, utility, PPR | FS `memory/memory.json` · SQLite · Postgres |
   | **Security / sessions** | grants, sealed turns | FS security.json + sessions/ · vaulted store |
   | **LLM** | answer synth | **hosted** BYOK · **mlx** OpenAI-compatible BYOC · none |
   | **Embed** | vectors | **hosted** Cohere · **mlx** · bag offline |
   | **Ranker / CE** | cross-encode | **hosted** ZE · lexical · (roadmap) local MLX CE |

5. **Same code path always** — modules call interfaces; substrates implement
   them. Switching gardener from SQLite to Postgres must not fork `ask` or
   invent a second cortex.

6. **Authority plane unchanged by this ADR** — ACL/Git/vault via
   `product-brain authority` remains a **separate control plane** (fail-closed
   process substrate). Residual substrate composition does **not** merge
   QueryService into residual ask. That is still STAGE-VS-PRODUCT doctrine.

## Consequences

### Positive

- Mental model matches code direction (`hosted.Client` + adapters).
- Laptop and team deploy share one pipeline; only durability/compute modules differ.
- Clear place to grow: implement `Queue` on Postgres without a “hosted product.”
- Docker becomes **optional packaging of remote substrates**, not a second brain.

### Costs / work

- Today several modules are still **implicitly bundled** (OpenLocal ⇒ FS chunks +
  SQLite gardener + FS cortex). Must evolve toward explicit binding.
- Neon opens often **omit** full cortex attach — contract parity gap.
- Docs that say “local path vs hosted path” need rephrasing toward substrates
  (OPS-PROFILES still useful as **preset bundles** of substrate choices).

### Explicit non-goals of this ADR

- Requiring every substrate combo to have identical latency or dense recall.
- Running two residual pipelines in one process for “comparison.”
- Replacing authority with residual substrate knobs.

## Current code vs target

| Concern | MVP now | Still open |
| --- | --- | --- |
| Process | One CLI binary | Same |
| Pipeline | One residual flow; always bind queue+cortex on solo | — |
| Chunk store | `--backend` / `chunks` fs\|memory\|neon | — |
| Gardener queue | sqlite default; **postgres** SKIP LOCKED; memory→sqlite; ephemeral tests | Multi-region queue |
| Cortex | `fs`\|`memory`\|`none`; FS default under Dir | Postgres cortex store |
| Dense | `none`\|`qdrant`\|`sqlite`\|`postgres`\|`faiss` HTTP\|`memory` | In-process FAISS CGo; pgvector `<=>` |
| Local workers | `OUROBOROS_BRAIN_WORKERS` / GOMAXPROCS; batch durable flush | Remote worker fleet |
| LLM / embed / ranker | `hosted`\|`mlx`\|`none` (chat/embed/rerank HTTP) | Model lifecycle packaging |
| Defaults | Hosted residual preferred when remote env present; solo offline | — |
| Config | `SubstrateConfig` + profile solo\|team\|bench + env | Optional YAML profile file |

## Preset profiles (bundles, not products)

Profiles are **named sets of substrate choices**, not alternate pipelines:

| Profile | Chunks | Queue | Cortex | Dense | Lexical serve |
| --- | --- | --- | --- | --- | --- |
| **solo** (laptop) | FS | SQLite (zero deps) | FS json | sqlite `dense.db` | HotLex gob |
| **solo-parallel** | FS | **Postgres** local | FS json | postgres or sqlite | HotLex gob |
| **team** | Neon product_* | Postgres | FS or Postgres | Qdrant / postgres | HotLex gob + hydrate |
| **bench** | path2 SMF | as configured | as wired | Qdrant | HotLex project / FTS |

See [OPS-PROFILES-LOCAL-HOSTED.md](../runbooks/OPS-PROFILES-LOCAL-HOSTED.md) for
operator checklists; that runbook describes **presets** under this ADR.

## SQLite vs Postgres (local parallel R/W)

| | SQLite | Postgres |
| --- | --- | --- |
| Install | None (file under Dir) | Local daemon or Neon |
| Readers | Concurrent (WAL) | Concurrent |
| Writers | **Serialized** (one writer) | Concurrent + `SKIP LOCKED` claims |
| Use | Default solo laptop | Local workers / team residual |

Prefer **Postgres** when `OUROBOROS_BRAIN_WORKERS` > 1 for gardener drain under load.
Prefer **SQLite** when you want zero ops and accept single-writer queue physics.

## Implementation sequence (when prioritized)

1. ~~Name modules in config~~  
2. ~~Always attach cortex + queue on residual open~~  
3. ~~Pluggable `gardener.Queue` (SQLite \| Postgres)~~  
4. Pluggable cortex store (json | sqlite | postgres) behind `memory.Store` API.  
5. Optional YAML profile file.  
6. ~~Delete language that implies two residual products~~ (ongoing honesty).

## Related

- ADR 0020–0022 (one Go product path, no dual engines)  
- ADR 0023 (unification program)  
- [OPS-PROFILES-LOCAL-HOSTED.md](../runbooks/OPS-PROFILES-LOCAL-HOSTED.md)  
- [STAGE-VS-PRODUCT.md](../specs/product/STAGE-VS-PRODUCT.md) (authority plane)  
- [REMAINING-GAPS.md](../roadmap/REMAINING-GAPS.md) (scale/delta store follow-ons)

## Status vocabulary

- **Accepted** as direction.  
- **Shipped (MVP)** in code: independent bind for chunks/queue/cortex; CLI verbs on
  one open path; unit + launch evidence for solo + one mix.  
- Still open: Postgres queue/cortex impls, optional YAML profile file — scale
  follow-ons, not a second residual product.
