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
`Report.RecordSavings(projectCache)` optionally appends those totals to the
local `internal/savings` ledger; omitting it preserves the existing report and
codeserve wire behavior.

## Retrieval QA suite (issue #48)

`qa.go` adds a deterministic retrieval-quality benchmark on top of the same
codeserve protocol. A `QASuite` is a set of probes with known expected files;
`RunQA` indexes the root and measures, per probe, hit@1/5/10 (rank of the first
expected file in `code_search` results), precision, latency, normalized
response bytes/tokens, and a failure class (`hit`/`miss`/`empty`/`error`).
`QAReport.CheckThresholds` gates on regression floors; `qaDigest` hashes the
latency-free core so the digest is stable across machines.

`QAFixtureSuite` and `QAFixtureThresholds` are the canonical probe set and
regression gates for the checked-in `testdata/qafixture` corpus. Retrieval
lanes are declared via `LaneLocalHeuristic` (measured) and the deferred
`LaneDenseReranked` / `LaneCompilerAuthority` constants so claims are never
attributed to an unimplemented lane. See `docs/benchmarks/bench-code.md` and
the `services/brain/cmd/bench-code` runner (multi-client smoke matrix,
baseline digest, artifact).

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
then `MeasureBaseline(root)`. Optionally call `RecordSavings(projectCache)` to
persist one scenario-level retrieval measurement without double-counting the
scenario baseline across protocol calls. Call `Report.Normalize(root, cache)`
before serializing to strip machine-local paths and zero timing fields so the
output is deterministic across machines. The resulting JSON artifact is
suitable for committing or diffing across phases.
