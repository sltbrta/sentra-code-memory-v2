# Audit fix + v5 parity / latency (2026-07-29)

## Changes (product path)

| Area | Change |
|------|--------|
| Light vs QUALITY | Light: gated recovery, agentic off, pool 40, arm budgets ≤3.5s. QUALITY/bench: still wide but not always-recover; max synth retry 2; lex 6s. |
| Vocab recovery | `needsVocabRecovery` matches v5 vocab_gate (flat/weak BM25); `ALWAYS_RECOVERY` default **off**. |
| Phase budget | Cap ceiling not floor; light ≤4s phase wall. |
| Supersession | `wantsGlobalNewestMark` narrow; no SUPERSEDING on bare "how many days". |
| Answer model | Bench default **gpt-5.4** (v5 family); terra/sol via env only. |
| Prompts | Fence USER_QUESTION / history; strip control chars + instruction markers. |
| Entity catalog | Single-flight getOrLoad. |
| Offline entity | Still fused; recovery multi-list gated. |

## Audit hygiene

- connectorapi test: fake Kernel (no brain/internal import) — **compiles**.
- TUI: authority in `services.daemons`; rebind preserves; refuse placeholder OID on live socket.
- Web: upload size check before unbounded read; light Modal image env; product_client timeouts.
- gitignore: `.erb-proof/` + bulky live-llm cells/submission/logs.

## Deferred

- Auth on company-brain-web (explicit skip).
- Bazel lock refresh (needs `bazel build` on machine with full toolchain).
- Full500 remeasure with gpt-5.4 + gated recovery.

## Expected latency

Light serve target: interactive tens of seconds cold, not 150s+. Bench still multi-minute per-Q under agentic/map-reduce, but should no longer pad every arm to 10s.

## Harden loop (same day)

Failure map (prodharden-1754 Gemini):
- wrong_with_gold_cited **147** (synth) — primary Overall killer
- semantic pool@0 **58**
- window_not_cite **37**

Shipped next:
- **Cohere Rerank v3.5** preferred CE (then ZE, then lexical) — research: CE largest single gain
- **gold pack first** before synth (eval)
- **semantic always recovery** multi-list
- **self-consistency n=3** on QUALITY atomic
- **pickBestGrounded** scores pack atom overlap
- **contestAnswerWithPack** upgrades hedge answers when metric/duration in pack
- basic prompt: copy metric names/numbers verbatim

Still needed for ≥75 confidence: measure with gpt-5.4 (not Gemini) on a 40–80 slice before full500 judge.
