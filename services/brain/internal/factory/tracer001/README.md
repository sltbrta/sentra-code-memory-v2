# Tracer 001 workflow compiler

Status: **[partial] Stage 06 L2 — typed DAG only.** Pure compiler for the
Tracer 001 one-layer factory workflow. No gateway, TUI, runner, or live GitHub.

## Why this path

The Stage 05 factory lives under `services/brain/internal/factory` (Go kernel)
and `services/broker/internal/factory` (runner/effects). There is no TypeScript
`services/factory/` tree. This package is the Stage 06 L2 compiler leaf that
the implementation packet originally named `services/factory/src/workflows/tracer-001/`.

## API

```go
workflow, err := tracer001.Compile(tracer001.CompileRequest{...})
workflow, err := tracer001.CompileFromHandoff(handoff, tenant, principal, session, runID, planID, policyDigest, review)
n, err := tracer001.SelectN(candidates, min, max)
err = tracer001.ValidateNoRedispatch(workflow)
err = tracer001.ValidateSealedActions(workflow)
```

## Invariants

| Invariant | Enforcement |
| --- | --- |
| `1 <= N <= 3` | `SelectN` + leaf validation |
| Prefix-disjoint leaf scopes | pairwise collision under APFS case-fold |
| Scopes attenuate approved intent | path-within-scope check |
| One-layer DAG | edges only from `orchestrator` |
| No leaf redispatch | `CanRedispatch=false`; leaf→leaf edges deny |
| Four required gates | BUILD, TEST, DOCS, SECURITY |
| No merge/deploy grant | sealed action set = `factory.leaf.execute` only |
| Deterministic digest | `WorkflowDigest` over canonical JSON projection |

## Tests

```text
bazel test //services/brain/internal/factory/tracer001:tracer001_test
```

Consumes L1 synthetic handoff facts from
`tests/fixtures/stage-06/tracer/change-intent.json` (immutable digests only).
