# product-brain

**Sole product CLI** (ADR 0022 / 0023). Residual company-doc IR + memory cortex
+
code operator + authority process.

## Command groups

| Group | Commands |
| --- | --- |
| Company-doc | `create` `ingest` `ask` `watch` `gardener` |
| Memory cortex | `memory …` · `ask --as-of/--known-at` · gardener `--lifecycle [--rem]` |
| Security | `ask --profile` `--principal` |
| Code | `code-*` `code-exact` `serve` |
| TUI | `tui` — single product pane (Brain/Ops/Work/System) |
| Authority | `authority --bootstrap …` |
| Tenant | `tenant create\|status\|list\|disable\|brain-create` |
| Federation | `federated-ask` |
| MLX | `mlx start\|stop\|status\|env` |

## CLI timing

Every verb (except help) prints wall-clock duration on **stderr**:

```text
{"event":"cli_timing","command":"ask","duration_ms":2412,"product_owned":true}
timing  ask              2412ms
```

Stdout JSON maps also include `duration_ms` + `cli_action` when emitted via
`emitJSON`. Disable: `OUROBOROS_CLI_TIMING=0`.

## Residual pipeline (short)

```text
ingest → retrieval_ready + light memory seed
  → async gardener enrich → RunCortexMaintenance
  → ask (multi-arm IR + memory rank arms)
```

Truth: [memory/README.md](../../internal/memory/README.md) ·
[gardener/doc.go](../../internal/gardener/doc.go) ·
[STAGE-VS-PRODUCT]
(../../../../docs/decisions/0022-product-only-retire-stage.md) ·
[REMAINING-GAPS](../../../../docs/roadmap/REMAINING-GAPS.md).
