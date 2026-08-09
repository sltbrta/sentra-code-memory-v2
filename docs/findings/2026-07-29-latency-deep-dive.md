<!-- markdownlint-disable MD013 MD024 MD033 -->

# Latency deep dive — dramatic improvements

**Date:** 2026-07-29  
**Status:** analysis + backlog (not all implemented)  
**Session:** [handover
closeout](../handover/2026-07-29-SESSION-HANDOVER-STRUCTURE-BRAIN-CLOSEOUT.md)
**Quality companion:** [hardening + failure
modes](2026-07-29-erb-hardening-and-failure-modes.md)

Goal: **lean/light interactive p50 ≪ 15s warm**, cold first-hit bounded, QUALITY
remains opt-in slow path.

---

## 1. Measured baselines (honest)

| Surface | Typical wall | Dominant cost |
| --- | --- | --- |
| Web light **cold** | 15–50s | Modal container cold start + HotLex mmap + first LLM |
| Web light **cache hit** | ~1–3s | answer cache on volume |
| Web light warm uncached | ~15–35s | retrieve + OpenAI synth |
| QUALITY random-40 p50 | ~64–73s | multi-arm HotLex + agentic + multi-synth |
| hard-8 QUALITY p50 | ~49s | same |

**Non-claims:** not official bench latency SLOs; experimental product.

---

## 2. Latency budget decomposition (lean path)

```text
cold start (Modal)          3–15s   once per scale-to-zero
HotLex gob mmap             2–8s    first ask in container
embed + dense ANN           0–3s    skipped if hot strong (single-doc)
HotLex BM25 multi-phrase    <0.5s   in-process; parallel wall≈max
Neon FTS (if not skipped)   1–4s    prod LexTimeout
structure SQL               0–2.5s  skip when hot strong single-doc
sibling hydrate             0–2s    skip when strong + texts OK
CE / retain                 <0.5s
LLM synth                   1–8s    network + model
```

Wall ≈ **max(cold, HotLex load) + sum(remaining sequential)**; hydrate∥structure
already parallel on residual.

---

## 3. Highest-ROI levers (dramatic)

### L1 — Keep answer cache hot **[shipped]**

- Volume-backed cache TTL 24h; cache hit ~2s on smoke v6.
- **Next:** pre-warm top-N demo questions on deploy; cache key must include
  mode+brain.

### L2 — Never pay QUALITY on light **[shipped]**

- `product_client._serve_env`: light = PROD lean, agentic off, structure 2.5s.
- Hard multi-doc UI types auto-escalate to deep — keep that list tight.

### L3 — HotLex strong early-exit **[shipped, refine]**

- Skip dense + Neon FTS + structure when strong **single-doc**.
- **Do not** early-exit on rare-id / INC / freeze (false-strong kills pool).
- **Next:** strong threshold calibration vs gold miss rate.

### L4 — Warm containers / min-containers

- Modal `min_containers=1` for demo window → kills cold start ($$).
- Or scheduled ping `/api/health` every 5m.

### L5 — HotLex load once per process **[shipped]**

- Loaded at client open; ensure web reuses long-lived process (uvicorn
  workers=1).
- Avoid re-execing product-brain-eval if moving to in-process Go later.

### L6 — Replace subprocess eval with in-process Go HTTP **[planned, large]**

- Today: web Python → spawn `product-brain-eval` per query (mmap + env each
  time).
- **Dramatic win:** long-running Go serve binary with HotLex resident → shave
  2–10s/query.

### L7 — Streaming synth / first-token UX

- Does not cut total work but perceived latency; optional SSE.

### L8 — QUALITY arms cost control

- LLM multi-query budget 1.2s fail-open (shipped).
- Cap agentic expand TopK and rounds (shipped).
- Never re-enable 60s lex timeout on QUALITY default (shipped 8s).

### L9 — Dense only when useful

- Interactive already skips dense when hot strong.
- QUALITY: prefer short phrase embeds over long Q (shipped pattern).

### L10 — Structure SQL fail-fast

- Detached budget; skip on light single-doc hot strong.
- **Next:** structure only if lexical gap high.

---

## 4. Mode matrix (latency policy)

| Mode | Target wall (warm) | Allowed arms |
| --- | --- | --- |
| **light** | p50 ≤ 20s, p95 ≤ 45s | HotLex≤2, dense?, FTS rare-only, structure skip-strong, no agentic, no LLM-MQ, synth×1 |
| **deep** | p50 ≤ 45s | + agentic multi-doc, deep hydrate INC/freeze, no QUALITY residual |
| **research** | 1–3 min OK | QUALITY + agentic + corrective + LLM-MQ |
| **bench/QUALITY** | measure quality first | full residual + MQ + pool 72 |

---

## 5. Instrumentation gaps

| Gap | Action |
| --- | --- |
| Web returns latency_ms but not stage split always | ensure diagnostics propagate hot_lex_ms, synth_ms, structure_ms to UI (partial) |
| Modal cold vs warm not labeled | stamp `container_generation` / cold_start bool |
| Cache hit ratio not in /api/version | expose last-N cache stats |

---

## 6. Anti-patterns (do not re-introduce)

1. `OUROBOROS_ERB_FORCE_PATH2_FTS=1` on web (kills HotLex).
2. QUALITY=1 for light mode.
3. Sequential hydrate then structure without parallel.
4. Unbounded agentic recursion.
5. LLM multi-query on every lean expand retrieve (`basic` type skip — shipped).

---

## 7. Suggested milestone order

1. **Prove** light warm p50 with stage diags on 20 demo Qs (instrument).
2. **Warm pool** min_containers or health ping.
3. **In-process Go serve** (largest structural win).
4. **Quality-only** residual arms stay opt-in; soft gold ≥0.93 hold.
5. Official latency SLO only after official judge path exists.
