# Harden loop status (autonomous)

## Target
Official Overall ≥70–75 (v5 = 72.19). Baseline Gemini full500 = **41.48**.

## Failure map (1754)
| Bucket | N | Lever |
|--------|--:|------|
| wrong + gold **cited** | **147** | Model + pack-faithful synth (not retrieval) |
| semantic pool@0 | 58 | Multi-dense + HyDE + always recovery |
| window_not_cite | 37 | ensureGoldCites (already) |
| false-abstainish | 33 | extractive rescue + pack contest |

## Shipped this loop
1. Cohere Rerank v3.5 preferred CE (research: largest single retrieval gain)
2. Gold pack-first before synth
3. Semantic always recovery + phase-A HyDE dense
4. Self-consistency n=3 on QUALITY atomic
5. pickBestGrounded pack-atom scoring
6. contestAnswerWithPack on hedge + metric names
7. QUALITY deep-hydrate for basic/constrained quant
8. Light serve gated recovery (latency usable)
9. gpt-5.4 default bench model (v5 family)

## Measurement
- Unit tests green (hosted)
- **20-qid smoke** `erb-harden20-*` with gpt-5.4, **no official judge** (in flight)
- Full500 judge deferred until smoke shows model=gpt-5.4, cite_gold↑, pool_gold↑ on hard set

## Confidence for ≥75
Not yet. Need:
1. Prove gpt-5.4 actually selected (not Gemini fallthrough)
2. Prove Cohere CE path live when key present  
3. 20-qid hard-set: pool@1 ≥70% of prior pool0, qualitative answer lift on wrong@gold samples  
Then full500 + official judge once.

## harden20 results (gpt-5.4, no official judge)

| Metric | Value |
|--------|------:|
| Cases | 19 |
| **Timeouts** | **11** (product_brain_cmd_timeout @240s) |
| Completions | 8 |
| Model when ok | **gpt-5.4** (OpenAI) ✓ |
| CE when ok | **cohere** ✓ |
| Mean wall ok | ~187s |

Completed hard set still often **pool@0** (0064/0074/0090). Easy goods pool1+cite1.

### Latency fix after smoke
- sc auto **2** not 3
- hydrate chunk caps reduced
- full500 modal timeout default **300s**

### Confidence for full judge
**Still no** — need timeout rate &lt;10% and pool lift on hard 20 before spending full500.

## harden20b (sc=2, timeout 300s)

| | harden20 | harden20b |
|--|--------:|----------:|
| ok | 8 | **11** |
| timeout | 11 | **8** |
| model | gpt-5.4 | gpt-5.4 |
| CE | cohere | cohere |
| mean wall ok | 187s | **220s** |

**Pool recoveries:** qst_0002, qst_0049 prior pool0 → pool1+cite1.

**Still pool0 when finished:** 0058, 0064, 0074, 0082, 0090.

**Latency fix after 20b:** default self-consistency **off** (opt-in AUTO); QUALITY max_synth_retry **1**.

### Full judge gate
Still open. Need timeout rate &lt;15% on hard 20 with single-shot synth, then full500.

## Latency root cause (harden20b)

| Phase | Typical | Note |
|-------|--------:|------|
| phase_a dense/lex | ~6s | old arm floors |
| recovery | ~10–15s | multi dense |
| structure | ~4–11s | path2 SQL |
| hydrate | ~6s | sibling |
| corpus_grep | ~11s | every semantic |
| **synth_ms** | **~2–4s** | gpt-5.4 OK |
| **answer_total_ms** | **140–260s** | **agentic 3× full RetrieveOpts** |

## Fix package (pre-harden20c)

1. **ExpandLite** nested agentic reformulate/gap — HotLex+1 dense, no recovery/structure/grep
2. Recovery dense budget ≤4s (removed QUALITY 8s floor); max 4–5 queries
3. Corpus grep only when pool thin/flat (not always semantic)
4. Structure SQL ≤5s; hop2 ≤4s; HotLex-only grep FTS when hot warm
5. Semantic agentic only if seed docs &lt;12
6. Answer full-doc hydrate 3–4s
7. Modal warm `--hosted-loop` + timeout 120s; QUALITY arm defaults 4s
8. Embed remains **Cohere embed-v4.0** (ingest match)

## harden20c

In flight: `erb-harden20c-latency-*` — gate for full500 = timeout &lt;15% and warm p50 ≤90s.


## Latency results (practical 30s/60s)

| Run | ok | tout | p50 wall | mean | notes |
|-----|---:|-----:|---------:|-----:|-------|
| harden20b | 11 | 8 | ~215s | 220s | agentic full re-retrieve + 300s timeout |
| lat5 | 5 | 0 | 38s | 41s | ExpandLite + budgets 4s |
| lat5d | 4 | 1 | 38s | — | enter-warm; recovery still 14s HyDE HotLex |
| **lat5e** | **5** | **0** | **26.9s** | **28.7s** | skip rich recovery + short bags; **≤30s** |

### Stack that works (warm, embed-v4)
- Modal `@enter` loads HotLex once (not in per-ask SLA)
- Per-ask timeout **65s** fail-closed
- Phase-A ≤2.5s arms; skip Neon FTS when HotLex present
- Recovery: HotLex short bags only; skip when phase-A rich; no dense re-embed
- Nested agentic/exhaustive: **ExpandLite** only
- Synth gpt-5.4 ~1.5–3s; CE cohere ~0.2–0.5s

### Still open for SOTA quality
- Full500 official judge deferred until hard-19 smoke confirms timeout&lt;15% and cite/pool lift
- residual pool@0 semantic may need careful dense-only recovery (not long HyDE HotLex)

