<!-- markdownlint-disable MD013 MD024 MD033 -->

# ERB product-path hardening targets + failure modes

**Date:** 2026-07-29  
**Status:** living target list (not a claim of completion)  
**Session:** [handover
2026-07-29](../handover/2026-07-29-SESSION-HANDOVER-STRUCTURE-BRAIN-CLOSEOUT.md)
**Latency companion:** [latency deep dive](2026-07-29-latency-deep-dive.md)

Soft gold only unless noted. **Not** official ERB judge / SOTA / promotion.

---

## 1. Current soft-gold floor (honest)

| Run | cite@1 | pool@1 | mean pool_recall | notes |
| --- | --- | --- | --- | --- |
| `erb-random40-20260729-0134` | 0.625 | 0.625 | 0.71 | baseline |
| `erb-random40-fix-20260729-0143` | 0.9375 | 0.9375 | 0.91 | multiquery/agentic/info fixes |
| `erb-random40-llmmq-20260729-0203` | 0.9375 | 0.9375 | 0.92 | + LLM multi-query (Groq×24) |
| hard-8 remaining v9 | 1.0 | 1.0 | — | earlier cite@90 hard set |

**Persistent soft-gold misses (random-40):** `qst_0202` (semantic), `qst_0414`
(conflicting_info).

---

## 2. Root causes already diagnosed

### F-0202 — lexicon + multi-chunk CRM

- Question: **spending freeze** date for EU finance/fraud opportunity.
- Gold `dsid_1faf80f5…` (Deepwell): **budget freeze** on **2026-01-20** lives on
  later chunk.
- BM25 on “spending freeze” ranks wrong finance docs; model invents dates.
- **Shipped mitigations (branch):** synonym expand (`budget freeze`, Deepwell
  bags); rare-id FTS; deep multi-chunk hydrate; `stripUngroundedFacts` for
  dates/money not in pack; prompt semantic note.

### F-0414 — conflict correction buried

- Question: INC-9821 OOM vs driver/kernel launch stalls.
- Gold pair includes late **telemetry correction**: stalls, **no sustained
  OOM**.
- Early thread is latency/5xx; HotLex fills with other GPU incidents.
- **Shipped mitigations:** INC-9821/Crucible/stalls bags; `INC-####` rare-id;
  conflicting deep hydrate (6 chunks); agentic hydrate expand; conflict prompt
  prefers superseding revision.

---

## 3. Hardening backlog (priority order)

### P0 — pool / cite correctness

| ID | Target | Mode surface | Fix shape |
| --- | --- | --- | --- |
| H-1 | Re-smoke 0202/0414 after synonym+hydrate; prove pool_gold_hits≥1 | QUALITY + deep | measure only |
| H-2 | Generalize freeze lexicon (procurement hold / budget freeze / spend freeze) without per-Q overfit | lean+deep | more static patterns + tests |
| H-3 | INC-#### always force FTS bag = ticket id alone (highest IDF) | lean+deep | retrieve FTS query pick |
| H-4 | Conflicting_info: dual-list RRF (early vs late phrase) | QUALITY/deep | agentic only to protect lean |
| H-5 | Soft fact_cov lag (0143 mean ~0.39) — multi-fact completeness lists | QUALITY | cite budget + completeness retry already; improve extractive ranking |

### P1 — grounding honesty

| ID | Target | Notes |
| --- | --- | --- |
| G-1 | Expand `stripUngroundedFacts` to ticket IDs / SLO numbers not in pack | after date/money |
| G-2 | When all claims fail quotes, prefer extractive span over free-form synth | answer.go path |
| G-3 | info_not_found: keep force caveat; never soft-pass mid-body “do not” | shipped; regression tests |
| G-4 | Company residual plane: never cite path2 dsids | plane honesty |

### P2 — agentic / deep / lean consistency

| ID | Target | Latency note |
| --- | --- | --- |
| M-1 | Lean: ≤2 HotLex bags parallel (shipped) — never LLM multi-query | lean RTT |
| M-2 | Deep/bench: agentic + deep hydrate; no QUALITY 60s lex | web product_client |
| M-3 | Research/QUALITY: LLM multi-query + wider pool | opt-in only |
| M-4 | Shared `groundAnswerInPassages` on all synth retries | shipped |

### P3 — infra / eval hygiene

| ID | Target |
| --- | --- |
| E-1 | Bazel memory BUILD.bazel must list `relations.go` (CI break) |
| E-2 | Official judge only with `OUROBOROS_ERB_OFFICIAL_JUDGE=1` + pins |
| E-3 | Modal secrets always include Cerebras/Groq for MQ when measuring MQ |
| E-4 | Soft gold compare script checked into tools/ for one-command re-run |

---

## 4. Failure-mode taxonomy

| Mode | Symptom | Primary lever |
| --- | --- | --- |
| **Paraphrase miss** | pool@0, gold uses different surface form | static expand + LLM MQ |
| **Multi-chunk late fact** | gold dsid in HotLex top but wrong chunk | deep hydrate / sibling 6+ |
| **False HotLex strong** | skips Neon FTS; rare ID miss | `hasRareIdentifier` override |
| **Incident collision** | other INC/GPU docs drown gold | ticket-only FTS + domain anchors |
| **Invented dates** | answer has ISO date not in pack | stripUngroundedFacts |
| **False abstain** | refuses with evidence present | type prompts + retries |
| **False answerable** | invents on info_not_found | force caveat + invent strip |
| **Cache lie** | stale answer after upload | volume commit/reload + cache key |
| **Lean budget burn** | light >60s | structure SQL skip when hot strong; no agentic |

---

## 5. Cross-mode checklist (do not regress)

| Capability | lean/light | deep/bench | research/QUALITY |
| --- | --- | --- | --- |
| HotLex multi-phrase | ≤2 parallel | full residual arms | full + LLM MQ |
| Agentic | off | multi-doc on | on + corrective opt-in |
| Deep hydrate (INC/freeze) | on (bounded) | on | on |
| Ungrounded date strip | on | on | on |
| Structure SQL | skip if hot strong single-doc | budgeted | budgeted |

---

## 6. Evidence pointers

- Random-40 soft gold:
  `docs/stages/stage-09/evidence/enterprise-rag-bench/live-llm/erb-random40-*.gold-compare.json`
- Web smoke: `…/web-smoke-all-20260729-v6.json`
- Hosted code:
  `services/brain/internal/hosted/{multiquery,retrieve,retrieve_interactive,agentic,ground,answer,llm_multiquery}.go`
- Web: `deploy/modal/company-brain-web/`
