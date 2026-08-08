<!-- markdownlint-disable MD013 -->

# Brain service

**Product-only** monorepo brain ([ADR 0022](../../docs/decisions/0022-product-only-retire-stage.md),
[ADR 0023](../../docs/decisions/0023-unified-product-durability-and-program-ladder.md)).

Maps: [ARCHITECTURE.md](../../ARCHITECTURE.md) ·
[program](../../docs/specs/product/program/README.md) ·
[deferred/non-goals](../../docs/roadmap/DEFERRED-AND-NON-GOALS.md) ·
[ops profiles local/hosted](../../docs/runbooks/OPS-PROFILES-LOCAL-HOSTED.md)

## Binary

`//services/brain/cmd/product-brain` — sole product entry.

```bash
# Company brain (residual IR + memory cortex + gardener)
product-brain create --dir <brain>
product-brain ingest --dir <brain> --jsonl docs.jsonl
product-brain ask --dir <brain> --q "…" [--profile single_user|multi_principal --principal P]
product-brain watch --dir <brain> --docs <path>
product-brain gardener --dir <brain> [--once|--lifecycle] [--rem] [--lifecycle-interval 30m]
product-brain watch --dir <brain> --docs <path> | --registry <file>
product-brain dense-bakeoff [--sizes 256,2048,8192] [--top-k 10] [--out receipt.json]
product-brain tui
# code-* is workspace operator; authority is ACL/Git substrate

# Code operator (not SCM session product)
product-brain code-index --root <src>
product-brain code-search --root <src> --q "…"
product-brain serve   # codeserve multi-verb JSONL

# Authority process (ACL / Git / vault / factory RPC)
product-brain authority --bootstrap /abs.json --bootstrap-sha256 <hex64>

# Multi-tenant MVP
product-brain tenant create --root <reg> --id t1
product-brain ask --tenant t1 --tenant-root <reg> --brain-id b1 --q "…"

# Federation MVP
product-brain federated-ask --q "…" --principal alice --cards path:id:allow

# Memory cortex (claims / episodes / agent notes / utility)
product-brain memory claim-admit --dir <brain> --subject S --predicate P --object O --docs d1
product-brain memory claim-list --dir <brain>
product-brain memory episode-list --dir <brain>
product-brain memory put|get|search --dir <brain> --principal alice --text "…"
product-brain gardener --dir <brain> --lifecycle   # C1 + cortex + utility + RAPTOR
```

Local brain dir (**FS projection retained** — agent-friendly):

`meta.json`, `chunks.jsonl`, `dense.db`, `sidecars.jsonl`, `memory/memory.json`
(bi-temporal claims + TemporalRelations), `security.json`, `hotlex.gob`,
`gardener.db`, optional `sessions/`.

`hotlex.gob` is now a historical filename for the versioned `HOTLEX2`
zero-decode mmap image. It is brain/generation scoped, checksummed, bounded,
and atomically published. A validated pre-migration gob is preserved at
`hotlex.gob.rollback.gob`; fresh projection cutovers can use `--rollback-gob`,
and `--format legacy-gob` is the explicit gob-only format switch. Legacy gob
images are otherwise accepted only for recovery and republish. See
[HotLex mmap snapshot](../../docs/specs/brain/HOTLEX-SNAPSHOT.md).

**Left-shift / lean ask:** burst ingest + async gardener compute structure,
HotLex, dense, d2q, cortex. Default serve ask is HotLex+dense+structure-SQL-hop
+CE (not unbounded multi-arm). QUALITY/research opt-in multi-arm.

**Hosted retrieval safety:** a missing or empty HotLex projection gets one Neon
FTS variant under a maximum 3s shared wall; parallel variants never multiply
that wall. Product/default and official runs never use an unbounded FTS context.
Dense/FTS phase-A fanout and path2 structure arms each share one child wall of
the request; hop-2 and hydration inherit the same caller cancellation. Diagnostics
keep `hot_lex_state=missing`, the full
`neon_fts_fallback_outcome=hits|empty|partial_failure|timeout|canceled|error|skipped|not_started`
set, and `retrieve_class=residual_opt_in` distinct. `*_caller_deadline_only=true`
marks the explicit non-official BENCHMAX zero-budget posture; near-deadline path2
work reports `path2_structure_budget_source=caller_deadline` and
`path2_structure_near_deadline=true`. `OUROBOROS_ERB_FORCE_FTS` cannot remove
the live bound; `OUROBOROS_ERB_FORCE_RESIDUAL` is an ablation route and is
reported as `retrieval_route_reason=force_residual`.

**ADR 0024 substrates:** one residual pipeline; modules pluggable
(`queue=sqlite|postgres`, `dense=sqlite|postgres|qdrant|faiss`,
`llm|embed|ranker=hosted|mlx|none`). Hosted preferred when configured.
**Local workers** (`OUROBOROS_BRAIN_WORKERS`) = burst fleet when residual is local.
Env `OUROBOROS_BRAIN_GARDENER_AUTO=1` starts background enrich+cortex after open.  
See [ARCHITECTURE](../../ARCHITECTURE.md) §3.2 ·
[structure-brain foundation](../../docs/research/2026-07-28-structure-brain-foundation.md) ·
[OPS-PROFILES](../../docs/runbooks/OPS-PROFILES-LOCAL-HOSTED.md).

## Package map (`internal/`)

| Package | README | Role |
| --- | --- | --- |
| [hosted](internal/hosted/) | yes | ONE residual company-doc Client |
| [gardener](internal/gardener/) | yes | Async enrich + lifecycle |
| [continual](internal/continual/) | yes | Doc watch → delta ingest |
| [codecrawl](internal/codecrawl/) | yes | Working-tree code index |
| [codeserve](internal/codeserve/) | yes | Multi-verb code protocol |
| [codeindex](internal/codeindex/) | yes | P5 exact syntax projections |
| [productsearch](internal/productsearch/) | yes | Search/Ask profile facade |
| [productsec](internal/productsec/) | yes | Security profiles + seal |
| [tenant](internal/tenant/) | yes | Multi-tenant registry MVP |
| [federation](internal/federation/) | yes | Federated ask MVP |
| [memory](internal/memory/) | yes | Cortex: claims, PageIndex, global PR prior, bipartite PPR, RAPTOR/community, agent tiers |
| [query](internal/query/) | yes | ACL-first grounded query engine |
| [conversation](internal/conversation/) | yes | Encrypted session vault (authority) |
| [ingestion](internal/ingestion/) | yes | Committed-Git generation publish |
| [connector](connector/) | yes | Active connector-owned composition boundary |
| [localauthority](localauthority/) | yes | Retained authority substrate; frozen against new connector product surfaces |
| [factory](internal/factory/) | yes | ChangeIntent DAG kernel |
| artifactvault / keyring / localstate | yes | Durability substrate |

Eval harness: `cmd/product-brain-eval` (official benches; promotion held).

## What “SCM session product” is *not*

Repo tools above are **code operator** parity. Agent **session continuation
memory** is deferred — see
[SCM-SESSION-PRODUCT.md](../../docs/specs/product/SCM-SESSION-PRODUCT.md).
