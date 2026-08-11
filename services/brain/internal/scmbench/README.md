# scmbench

Deterministic, offline benchmark scaffold for the local-first SCM
code-memory workflow (Phase 0: contracts, fixtures, benchmarks).

## What it measures

Per workflow step through the `codeserve` protocol:

- normalized response bytes (wire JSON with local paths/timings scrubbed for comparison)
- estimated tokens (`EstimateTokens`: fixed 4-bytes-per-token yardstick)
- tool calls (one per protocol step)
- latency in milliseconds (recorded, never asserted)

`Report.MeasureBaseline` compares the workflow against the naive
"read the whole tree" agent baseline and reports byte/token savings.

## Guarantees

- No network providers, hosted inference, or cloud anything.
- Deterministic fixtures and token accounting; safe to diff reports.
- Latency is observational only and excluded from normalized report comparisons.

## Usage

```sh
go test ./services/brain/internal/scmbench/                    # conformance
go test ./services/brain/internal/scmbench/ -bench . -benchtime 20x
```

Build a `Scenario` (root, index cache, ordered verb steps), call `Run`,
then `MeasureBaseline(root)`. Call `Report.Normalize(root, cache)` before
serializing to strip machine-local paths and zero timing fields so the
output is deterministic across machines. The resulting JSON artifact is
suitable for committing or diffing across phases.
