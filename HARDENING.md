# Hardening ledger

Appended whenever a wave defers a check. Deferred is not skipped: this ledger
is what makes deferring safe. Entering W3 means draining it.

Schema per entry: date, wave, trigger, why deferred, what would satisfy it.

## Open

### 2026-08-21 — `TestFrozenExactly100ChangeFixture` is load-sensitive

- **Wave:** W3
- **Trigger:** failed once during a full `go test ./...` run, passed on two
  subsequent full runs and on seven isolated runs of its own package, both at
  this branch and at the base revision. Not caused by the trust-gate change,
  which does not touch `ingestion`.
- **Why deferred:** unreproducible on demand. Chasing it now would spend the
  batch's repair budget on a symptom rather than the finding.
- **What would satisfy it:** the package's fixture helper uses a blocking git
  wrapper polled by `waitForFile` with a fixed 5s deadline
  (`ingestion/test_helpers_test.go`). Replace the wall-clock deadline with a
  synchronising channel, or raise it under `-race`/parallel load, then run
  `go test -count=20 ./brain/internal/ingestion/` under CPU contention.

## Drained

### 2026-08-21 — no automated CI existed

- **Wave:** W3
- **Trigger:** the 166-finding audit found no `.github/`, no pipeline; every
  gate was a manual `just` invocation, and 39 of 87 `.local-agent-ci` receipts
  were policy escapes.
- **Why deferred:** never recorded as deferred — it was simply absent, which is
  the failure mode this ledger exists to prevent.
- **What satisfied it:** `.github/workflows/ci.yml` running build, vet, gofmt,
  `test -race -count=1`, the generated-contract drift check, govulncheck, cargo,
  and `bench-code` on every push.
