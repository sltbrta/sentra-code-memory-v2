# Hardening ledger

Appended whenever a wave defers a check. Deferred is not skipped: this ledger
is what makes deferring safe. Entering W3 means draining it.

Schema per entry: date, wave, trigger, why deferred, what would satisfy it.

## Open

### 2026-08-21 — factory BUILD and TEST gates do not build or test

- **Wave:** W3
- **Trigger:** the gates named `FACTORY_GATE_KIND_BUILD` and
  `FACTORY_GATE_KIND_TEST` check that a leaf reached `COMPLETED` and that
  touched Go files parse. Callers read `FACTORY_GATE_STATUS_PASSED` as an
  assurance that a change set builds and its tests pass. Non-Go edits skip both
  gates entirely and pass having been checked by nothing.
- **Why deferred:** not a bodge away, a design gap. `evaluateFactoryGate`
  receives `leaf.outcome.Edits` -- in-memory post-image bytes -- and has neither
  the repository root nor an execution sandbox. A package does not compile in
  isolation from its module, so there is nothing to run `go build` against.
  Implementing it inside the current signature would mean compiling a fragment
  and calling the result a build, which is the same overclaiming in a new form.
- **What would satisfy it:** materialise the edits as a `go build -overlay`
  overlay file against the real repository root, and run `go build ./...` and
  `go test ./...` through it under `exec.CommandContext` with a timeout, a
  scrubbed environment and a process-group kill -- the same discipline the
  change-set verification gate now uses (`workflow/verification_command.go`).
  That requires threading the repository root and a runner port into the
  pipeline. The DOCS gate was in reach and is now real: it checks that every
  exported declaration in every touched Go file carries a doc comment, rather
  than asking whether the file contains the characters `//`.

### 2026-08-21 — savings figures are estimates presented as measurements

- **Wave:** W3
- **Trigger:** `savings` reports `baseline_tokens` / `served_tokens` /
  `saved_tokens` derived from a flat bytes÷4 heuristic, against a baseline of
  "an agent reads every indexed source file" that no real agent performs. The
  JSON and CLI carry no qualifier. Separately the ledger has no producer in the
  product: only the benchmark writes to it, so `savings_summary` answers
  `steps: 0` after real use.
- **Why deferred:** a real tokenizer means either vendoring a BPE vocabulary
  (a dependency decision recorded in `DECISIONS.md`) or shipping a model-specific
  approximation, and an honest baseline means measuring what an agent would
  otherwise have read per query rather than the whole repository. Both are
  design choices rather than corrections.
- **What would satisfy it:** rename the wire fields to `*_tokens_est`, emit the
  estimator identity and baseline model alongside them, have the retrieval verbs
  record a `savings.Step` per served response, and base the baseline on the gold
  files for a query rather than the tree.

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
