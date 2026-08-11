# scmbench

Deterministic, offline benchmark scaffold for the local-first SCM
code-memory workflow (Phase 0: contracts, fixtures, benchmarks).

## What it measures

Per workflow step through the `codeserve` protocol:

- response bytes (exact, from the marshaled wire response)
- estimated tokens (`EstimateTokens`: fixed 4-bytes-per-token yardstick)
- tool calls (one per protocol step)
- latency in milliseconds (recorded, never asserted)

`Report.MeasureBaseline` compares the workflow against the naive
"read the whole tree" agent baseline and reports byte/token savings.

## Guarantees

- No network providers, hosted inference, or cloud anything.
- Deterministic fixtures and token accounting; safe to diff reports.
- Latency is observational only — no timing assertions in tests.

## Usage

```sh
go test ./services/brain/internal/scmbench/                    # conformance
go test ./services/brain/internal/scmbench/ -bench . -benchtime 20x
```

Build a `Scenario` (root, index cache, ordered verb steps), call `Run`,
then `MeasureBaseline(root)`. The resulting `Report` marshals to a stable
JSON artifact suitable for committing or diffing across phases.
