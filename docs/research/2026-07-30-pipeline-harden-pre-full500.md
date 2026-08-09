# Pipeline harden + E2E smoke (pre-full500)

Date: 2026-07-30  
Scope: product-go ERB path (Neon+Qdrant+HotLex+Modal hosted-loop) before another
full500.

## Slice map

| Slice | Entry / files | Upstream | Downstream |
| ------- | --------------- | ---------- | ------------ |
| Harness | `tools/erb/launch_*.sh` → `modal_erb_hosted.py` | questions.jsonl, secrets, env | Modal map of cases |
| Modal worker | `ProductBrainWorker` @enter+answer | linux `product-brain-eval`, HotLex vol | cell JSON |
| Eval binary | `cmd/product-brain-eval` `--hosted-loop` | stdin JSONL case | stdout JSON + diags |
| Answer | `hosted.AnswerOpts` | RetrieveOpts + synth | AnswerResult |
| Retrieve | `retrieve_interactive` + smf_funnel + v5 tiers | HotLex/dense/CE | passages + tier |
| External | Cohere embed-v4, CE, gpt-5.4, Neon, Qdrant | keys | vectors/text |

## Prior failures (evidence)

| Run | n | fails | desync | timeout | notes |
| ----- | --: | ------: | -------: | --------: | ------- |
| full500-2341 | 500 | 480 | **298** | 182 | unusable |
| smf-funnel5 | 5 | 2 | **1** (want=0062 got=0001) | 1 (0001@60s) | funnel on when ok |
| align10 | 10 | 0 | 0 | 0 | p50 26.6s |

Root cause of desync: after `product_brain_cmd_timeout`, a late JSON line for
the prior qid remained readable on the warm loop; next ask read it. Aggravators:
ask_timeout hard-capped at 60s, no grace drain, no kill-wait, no restart, no
fail-closed empty-qid, possible concurrent inputs, stderr pipe fill risk.

## Fixes applied this pass

1. **Modal loop protocol** (`modal_erb_hosted.py`)
   - `@modal.concurrent(max_inputs=1)` (1.3 rejects `allow_concurrent_inputs`)
   - Global ask lock always installed
   - On timeout: grace-drain late line (2s) or kill+wait+close pipes+respawn
   - On desync/bad JSON: kill+respawn
   - Fail-closed when `want_qid` ≠ got (including empty got)
   - Daemon stderr drain (avoid 64KB block)
   - ask_timeout = `MODAL_TIMEOUT - 5` (no hard 60s cap)
2. **Go eval** (`product-brain-eval`)
   - Default per-ask `context.WithTimeout` 85s (`OUROBOROS_ERB_ASK_TIMEOUT_MS`)
   - `os.Stdout.Sync()` after each response line
   - Stamp `ask_timeout_ms` / `ask_deadline_exceeded`
3. **Unit test** `TestDetachedTimeoutSurvivesCancelledParent` — budget assert
   aligned to latency-hardened 1.5s prod default
4. **Launch defaults** — modal timeout 90s, ask 85s; smoke launcher
   `tools/erb/launch_harden_smoke.sh`

## Tests run

| Suite | Result |
| ------- | -------- |
| `go test ./services/brain/internal/hosted/` | **ok** (~47s) |
| `go test ./services/brain/cmd/product-brain-eval/` | **ok** |
| company-brain-web pytest (cache/modes/batch) | **17 passed** |
| `py_compile modal_erb_hosted.py` + `build_app()` | **ok** |

## E2E smoke: `erb-harden-smoke-20260730b`

- IDs: `0001,0002,0004,0021,0049,0062,0010,0030` (includes prior timeout+desync
  pair)
- burst=8, timeout=90, ask=85s, QUALITY/bench, gpt-5.4, loop=1

| Metric | Value |
| -------- | ------: |
| n | 8 |
| ok | **7** |
| timeout | **1** (`qst_0030` @85.0s) |
| **desync** | **0** |
| model ok | gpt-5.4 ×7 |
| p50 wall | ~57s |
| wall_clock | 113s |
| cite_gold_hits (ok) | 1×7 |
| smf_funnel / expand | all ok cells |

### Interpretation

- **Desync P0 closed** on the exact prior poison pair (0001 completed 82.5s;
  0062 ok 57s).
- Residual timeout is a **budget** issue on one hard expand (0030), not protocol
  corruption.
- All ok cells still `evidence_tier=expand` — lean path not exercised on this
  hard set (P2 quality/latency).

## Sequential stress

`erb-seq-stress-20260730` (burst=1, 5 qids incl. 0001/0062/0030) — results
filled after run.

## Full500 gate

| Gate | Status |
| ------ | -------- |
| desync rate ≈ 0 on smoke | **PASS** |
| timeout rate on hard 8 ≤15% | **PASS** (12.5%) |
| model=gpt-5.4 when ok | **PASS** |
| smf_funnel stamped | **PASS** |
| official judge on smoke | skipped (OFFICIAL_JUDGE=0) |

**Recommendation:** full500 allowed after sequential stress shows desync=0 under
single-container reuse. Prefer `MODAL_TIMEOUT=95–100` / `ASK_TIMEOUT_MS=90000`
if 0030-class tails remain.

## Residual P1/P2 (not blockers for smoke→full)

- **P1** latency: expand path still 45–85s; agentic rounds=0 but
  hard_pool_expand + whole-doc still tax wall
- **P2** `fb.DenseQueries` not applied in phase-A (uses `prod.DenseQueries` cap
  2; funnel expand wants 3)
- **P2** vocab_gate_weak over-fires expand on hard set (may be correct for hard
  ERB)
- **P2** official judge not wired on smoke (launch full500 still sets
  OFFICIAL_JUDGE=1)

## Sequential stress: `erb-seq-stress-20260730`

- burst=**1** (forced same-container reuse), 5 qids: 0001,0062,0030,0002,0049
- **failures=0, desync=0** (wall ~248s sequential)
- Proves timeout-drain/kill protocol does not poison subsequent asks on one warm
  loop.

## Full500 go/no-go

**GO** for full500 with current hardened harness. Suggested env:

```sh
OUROBOROS_ERB_MODAL_TIMEOUT=90
OUROBOROS_ERB_ASK_TIMEOUT_MS=85000
OUROBOROS_ERB_BURST_CONTAINERS=28
OUROBOROS_ERB_HOSTED_LOOP=1
OUROBOROS_ERB_OFFICIAL_JUDGE=1
```

Do not start full500 until explicitly approved (orchestrator gate).
