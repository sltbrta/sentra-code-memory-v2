# Hardening ledger

Appended whenever a wave defers a check. Deferred is not skipped: this ledger
is what makes deferring safe. Entering W3 means draining it.

Schema per entry: date, wave, trigger, why deferred, what would satisfy it.

## Open

None yet. Entries are appended as batches defer work.

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
