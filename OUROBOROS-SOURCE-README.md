<!-- markdownlint-disable MD013 MD033 -->

# Ouroboros

**Local-first company brain and software factory** — humans and models share
authorized evidence, grounded answers, code intelligence, and governed change
under one product brand.

| | |
| --- | --- |
| **Sole product binary** | `product-brain` |
| **Status** | Residual company brain + code operator + authority **shipped**; TUI **shipped**; GAP-PLANE honesty (`planes`) + residual Tier A product rows **closed**; CLI timers on every verb |
| **Doctrine** | ADR 0020–0024 — one Go product path; Stage-named packages = authority clients, not a dual CLI |
| **Trunk** | `main` only — PR-gated; local+remote single tip |

---

## What it is

```text
                    product-brain  (ONE binary)
         ┌──────────────────┼──────────────────┐
         │                  │                  │
   COMPANY BRAIN      CODE OPERATOR      AUTHORITY
   residual IR        working tree       ACL / Git / vault
   + memory cortex    + exact P5         factory / meeting
   + gardener         codecrawl/serve    peer Unix RPC
```

| Plane | Owns | Entry |
| --- | --- | --- |
| **Company residual** | Docs → left-shift build + lean serve ask; memory cortex; async gardener | `create` `ingest` `ask` `watch` `gardener` `memory` |
| **Code operator** | Working-tree index, search, impact/route, codeserve | `code-*` `code-exact` `serve` |
| **Authority** | Fail-closed ACL, committed Git, encrypted vault, factory RPC | `authority` |

They share one CLI brand. They are **not** one retrieve engine (by design).

---

## Quick start (company residual)

```bash
# From repo (Go workspace):
go build -o product-brain ./services/brain/cmd/product-brain

B=/tmp/my-brain
./product-brain create --dir "$B"
./product-brain ingest --dir "$B" --jsonl docs.jsonl   # retrieval_ready immediately
./product-brain gardener --dir "$B" --once            # enrich + RunCortexMaintenance
./product-brain ask --dir "$B" --q "What is the Widget price?"
# Every verb prints timing on stderr, e.g.:
#   {"event":"cli_timing","command":"ask","duration_ms":2400,"product_owned":true}
#   timing  ask              2400ms
# Disable: OUROBOROS_CLI_TIMING=0
```

**Pipeline (truth) — left-shift; structure encodes intelligence:**

```text
ingest (retrieval_ready immediately)
  light seed: utility + texts + episode · product structure co-occur · HotLex/dense when bound
  → async gardener.db (d2q / edges / context)
  → gardener --once | loop | --lifecycle
       RunCortexMaintenance: claims · TemporalRelations · edges · PageIndex · PR · RAPTOR
  → LEAN ask (default PROD/serve):
       HotLex + dense → RRF → structure SQL + ExpandRelations → CE → ground + dual-cite
  → QUALITY/research (opt-in): EvidenceTier lean|standard|expand + smf funnel budgets
       recovery/whole-doc only when weak; agentic ExpandLite (no full re-retrieve)
  → ERB Modal: hosted-loop (HotLex @enter) + qid fail-closed; smoke before full500
```

Local brain dir (**FS projection retained**):

`meta.json` · `chunks.jsonl` · `chunks.delta.jsonl` · `dense.db` · `sidecars.jsonl` ·
`hotlex.gob` (`HOTLEX2` mmap image) · `gardener.db` · `memory/memory.json` (claims + TemporalRelations) ·
`security.json` · optional `sessions/`

**Substrates (not two products):** [ADR 0024](docs/decisions/0024-one-pipeline-substrate-modules.md) —
hosted preferred when keys/URLs present; solo/FS offline; **Postgres queue/dense**
when you want parallel local workers; **MLX** for local LLM/embed/ranker BYOC.

### TUI (single product pane)

```bash
cd apps/tui && bun packages/shell/src/cli.ts
# or: product-brain tui
# Outside monorepo: OUROBOROS_TUI_ENTRY=/path/to/cli.ts
#   or: echo /path/to/cli.ts > ~/.ouroboros/tui-entry
#
# One pane: Brain · Ops · Work · System  (not multi-product tabs)
# Startup: secrets.env only + gardener (+ watch if folders)
# Mode/save: restarts session daemons · Quit: kills TUI-owned children
# Settings: mode local_solo|local_mlx|hosted_neon · keys never in tui-settings.json
# Spec: docs/specs/product/tui.md · apps/tui/packages/shell/README.md
```

Secrets live only in `~/.ouroboros/secrets.env` (mode 0600).  
Always-on OS packaging (XOR with TUI session): `deploy/scripts/install-continuous.sh`.  
Dense exact-vs-ANN receipt: `product-brain dense-bakeoff --sizes 256,2048,8192 --top-k 10`.
Dual-plane honesty: `product-brain planes` (residual ≠ authority ≠ code-exact).

### Sentra company-brain web (Modal demo)

Live: `https://sentra--ouroboros-brain.modal.run`  
Code: [`deploy/modal/company-brain-web/`](deploy/modal/company-brain-web/)

- **Ask ERB** — pre-ingested `full-bench-v2` (HotLex + Neon/Qdrant **one** product-go path)
- **Your docs** — sticky company brain (upload/append until Reset); PDF text extract
- **Batch asks** — one full question per line (Enter-separated); concurrency ≤8
- **Citations** — `cited_document_ids` + grounded claim quotes (text/quote/document_id)
  page/section/line locator when the source parser attached one
  (no synthetic locators); unsurfaced claims are filtered out at the wire
  boundary so the UI never sees claims whose document_id isn't surfaced
- Modes: **Light** (default lean) · **Deep** · **Research**
- Answer cache ~24h on volume (repeat asks ~1–3s)
- Experimental: cold starts and network limits affect latency

```bash
cd deploy/modal/company-brain-web
# rebuild linux binary into bin/ then:
modal deploy modal_app.py
python3 smoke_web_all.py --base https://sentra--ouroboros-brain.modal.run
```

Useful env:

| Variable | Effect |
| --- | --- |
| `OUROBOROS_BRAIN_ENRICH` | `async` (default with queue) / `sync` / `0` |
| `OUROBOROS_BRAIN_GARDENER_AUTO=1` | Background drain after open |
| `OUROBOROS_BRAIN_WORKERS=N` | Local burst + gardener concurrency (default GOMAXPROCS) |
| `OUROBOROS_BRAIN_QUEUE=postgres` + `QUEUE_DSN` | Parallel durable queue (else sqlite) |
| `OUROBOROS_BRAIN_DENSE=sqlite`\|`postgres`\|`qdrant`\|`faiss` | Dense ANN (qdrant needs `QDRANT_URL` + `QDRANT_API_KEY`) |
| `OUROBOROS_BRAIN_LLM`/`EMBED`/`RANKER` | `hosted`\|`mlx`\|`none` |
| `OUROBOROS_BRAIN_PAGEINDEX_LLM=1` | Agentic PageIndex walk (real user question) |
| `OUROBOROS_BRAIN_OPENIE_LLM=1` | LLM OpenIE triples (+ span offsets) |
| `OUROBOROS_BRAIN_GRAPHRAG_LLM=1` | GraphRAG abstractive reduce |
| `OUROBOROS_ERB_PROD` / `QUALITY` | Lean vs full residual IR |
| `OUROBOROS_ERB_FAITHFULNESS=0` | Roll back the default-on final answer-faithfulness gate |
| `OUROBOROS_BRAIN_PPR=0` | Disable multi-hop PPR boost |
| `OUROBOROS_BRAIN_REM=1` / `--rem` | Opt-in deterministic REM re-extract |
| `OUROBOROS_TUI_ENTRY` | Absolute path to TUI `cli.ts` when not in monorepo |

---

## Start here (docs)

| Doc | Role |
| --- | --- |
| **[ARCHITECTURE.md](ARCHITECTURE.md)** | Full monorepo pipeline map (left-shift §3.2; tiers/funnel §3.2) |
| **[Session handover (2026-07-30)](docs/handover/2026-07-30-SESSION-HANDOVER.md)** | **Current state, smoke gates, full500 next** |
| **[Pipeline harden pre-full500](docs/research/2026-07-30-pipeline-harden-pre-full500.md)** | Desync RCA + E2E smoke results |
| **[Structure-brain foundation](docs/research/2026-07-28-structure-brain-foundation.md)** | Stage gates S0–S6; pipeline adds/removes |
| **[ERB hardening + failure modes](docs/findings/2026-07-29-erb-hardening-and-failure-modes.md)** | Pool/cite/grounding fix targets |
| **[Latency deep dive](docs/findings/2026-07-29-latency-deep-dive.md)** | Dramatic latency levers (lean vs QUALITY) |
| **[Sentra company-brain web](deploy/modal/company-brain-web/README.md)** | Modal UI — ERB ask + sticky company upload; citations; light theme |
| **[Open inventory + value](docs/roadmap/OPEN-AND-VALUE.md)** | Everything still open + how it helps |
| **[Ops profiles / substrates](docs/runbooks/OPS-PROFILES-LOCAL-HOSTED.md)** | Substrate presets (solo/team/bench); not two products |
| **[ADR 0024 — one pipeline, module substrates](docs/decisions/0024-one-pipeline-substrate-modules.md)** | One process/flow; local vs remote only per module |
| **[Remaining gaps walkthrough](docs/roadmap/REMAINING-GAPS.md)** | Every open / partial / non-goal gap |
| **[Deferred & non-goals](docs/roadmap/DEFERRED-AND-NON-GOALS.md)** | NG-* + DEF summary + MVP partials |
| **[Memory cortex](services/brain/internal/memory/README.md)** | Bi-temporal claims + TemporalRelations + cortex |
| **[Brain service](services/brain/README.md)** | Package map + full CLI |
| **[Stage vs product](docs/specs/product/STAGE-VS-PRODUCT.md)** | Residual vs authority vs code |
| **[SCM session product](docs/specs/product/SCM-SESSION-PRODUCT.md)** | Deferred — ≠ code operator |
| **[Docs index](docs/README.md)** | Full documentation map |
| **[SPEC.md](SPEC.md)** | Specification spine |
| **[ROLLING.md](ROLLING.md)** | Append-only decisions log |

---

## Truth status (honest)

| Surface | Status |
| --- | --- |
| **product-brain** sole binary | **[shipped]** |
| Residual company IR (`hosted`) | **[shipped]** lean serve + QUALITY EvidenceTier/smf funnel + ExpandLite agentic + LLM multi-query opt-in |
| Memory cortex | **[shipped]** bi-temporal claims + TemporalRelations + conflict; LLM depth opt-in |
| Gardener async + lifecycle | **[shipped]**; LLM REM / continuous sleep product **non-goal** |
| Code operator (codecrawl / codeserve / code-exact) | **[shipped]**; **session product deferred** |
| Authority process substrate | **[shipped]** via `product-brain authority` |
| Multi-tenant / federation MVP | **[shipped]** local-file / local multi-brain |
| Shared full-screen TUI | **[partial]** residual shell shipped (`product-brain tui`); factory views planned |
| **Sentra company-brain web** (Modal) | **[partial]** live: batch asks, white Sentra UI, citations, sticky brains, answer cache; same product-go path; experimental latency |
| ERB Modal hosted-loop | **[shipped]** warm HotLex @enter; desync protocol hardened (smoke desync=0) |
| ERB soft gold / smoke | **[partial]** harden-smoke 7/8 + seq-stress 5/5 desync=0; soft gold ≠ official |
| Full-500 ERB / SOTA promotion | **[deferred]** fail-closed until clean full500 + official judge (2341 invalid) |
| SCM session continuation packets | **[deferred]** (different product class) |

Vocabulary: `[shipped]` · `[partial]` · `[scaffold]` · `[planned]` · `[deferred]`.
Underclaim preferred.

---

## Repository layout

```text
Ouroboros/
├── ARCHITECTURE.md          # Live system map
├── SPEC.md                  # Spec spine
├── README.md                # ← you are here
├── apps/                    # TUI, spotlight-mac
├── services/
│   ├── brain/               # product-brain + residual + authority libs
│   ├── broker/              # authz / identity / connectors
│   └── gateway/             # authorityprocess
├── docs/                    # Specs, ADRs, research, roadmap, handover
├── packages/contracts/      # Protobuf / generated types
├── archive/                 # Frozen Stage / Python duals (do not import)
├── workers/                 # Rust code-index, Python eval harness
└── tools/                   # Build spine, Modal, stage exit
```

---

## Develop / test

```bash
# Brain unit tests (residual cortex + hosted + gardener)
go test ./services/brain/internal/memory/ \
        ./services/brain/internal/hosted/ \
        ./services/brain/internal/gardener/ \
        ./services/brain/internal/ontology/ -count=1

# Contributor build facade (Stage 01) — see docs/runbooks/build-facade.md
```

Bazel targets and stage-exit gates remain the contributor/CI surface; product
runtime for company-doc is primarily Go `product-brain`.

---

## Explicit non-goals (do not “just build”)

- Second product binary or Python dual retrieval engines  
- Mem0/Zep/Graphiti as the product brain  
- SCM session continuation packets without un-deferring NG-SCM-010  
- Default unbounded LLM REM / sleep  
- Claiming LongMemEval or ERB full-500 SOTA without authorized suite  

Full list: [OPEN-AND-VALUE.md](docs/roadmap/OPEN-AND-VALUE.md) (prioritize) ·
[REMAINING-GAPS.md](docs/roadmap/REMAINING-GAPS.md) (status) ·
[DEFERRED-AND-NON-GOALS.md](docs/roadmap/DEFERRED-AND-NON-GOALS.md)

---

## License / ownership

Private product monorepo (Sammy / `@sltbrta`). Donor repos (SFS, Lattice, SCM, …)
are **concept sources only** — reimplement natively; no wholesale transplant.

Built with multi-agent harnesses; see project docs for agent contracts.
