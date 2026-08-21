# Hardening ledger

Appended whenever a wave defers a check. Deferred is not skipped: this ledger
is what makes deferring safe. Entering W3 means draining it.

Schema per entry: date, wave, trigger, why deferred, what would satisfy it.

## Open

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

### 2026-08-21 — performance findings not addressed

- **Wave:** W3
- **Trigger:** the audit found real performance defects that were confirmed by
  reading but not fixed in this pass: `code_exact` re-reads and re-parses the
  whole repository per query with no index or cache; there is no in-process
  index cache, so every verb call gob-decodes the entire index under a file
  lock; HNSW upsert is a linear id scan making load O(n²) and every batch
  rewrites the whole file; the reranker sees only the first 1,500 bytes of each
  document, which on code is the licence header and imports.
- **Why deferred:** each is a behaviour-preserving optimisation whose risk is a
  retrieval-quality regression, and the branch already carries a large
  correctness diff. Mixing them in would make a bisect harder precisely where
  the quality gate matters.
- **What would satisfy it:** take them one at a time behind `just bench-code`,
  with a `benchstat` comparison against the current numbers recorded in the
  amplification receipt. The reranker window in particular needs a quality run,
  not just a speed one — chunking around candidate definitions changes what is
  scored, not only how fast.

### 2026-08-21 — `TestFrozenExactly100ChangeFixture` is load-sensitive

**Still open: the failure has not been reproduced, so nothing here is proved.**

- **Wave:** W3
- **Trigger:** failed once during a full `go test ./...` run, passed on two
  subsequent full runs and on seven isolated runs of its own package, both at
  this branch and at the base revision. Not caused by the trust-gate change,
  which does not touch `ingestion`.
- **Why deferred:** unreproducible on demand.
- **Work done (2026-08-21):** the mechanism was located but not confirmed. The
  original entry pointed at `waitForFile`'s fixed 5s deadline, which this test
  does not call. The bound that expires first is `testConfig`'s
  `CommandTimeout: 10 * time.Second`, which bounds every git subprocess the
  authority spawns; a reconcile over the frozen 100-change fixture runs many,
  and the failure would surface as a context deadline in production code
  rather than as an obviously test-shaped timeout. Both allowances now scale
  through `testAllowance`, ×8 under the race detector and ×2 otherwise, and
  `waitForFile` additionally never waits past the test binary's own budget so
  a hang reports the filename instead of a goroutine dump.
- **Why it is not drained:** an attempt to reproduce failed. The package was
  run at `-count=20 -race` under 12 competing CPU-burning processes, twice --
  once with the scaled allowances and once with the original flat ones -- and
  both passed (222s and 263s). The scaled allowances are therefore a defensive
  change with no red proof behind them, which is not evidence that the
  original failure is fixed. Draining this entry would be the same
  overclaiming the branch exists to remove.
- **What would satisfy it:** a reproduction. Failing that, the next occurrence
  should be captured with `-race` and the failing output kept: if it is a
  `context deadline exceeded` from a git command the diagnosis above is
  confirmed and the entry can close; if it is anything else, this work was
  aimed at the wrong bound.

## Drained

### 2026-08-21 — factory BUILD and TEST gates did not build or test

- **Wave:** W3
- **Trigger:** the gates named `FACTORY_GATE_KIND_BUILD` and
  `FACTORY_GATE_KIND_TEST` checked that a leaf reached `COMPLETED` and that
  touched Go files parse. Callers read `FACTORY_GATE_STATUS_PASSED` as an
  assurance that a change set builds and its tests pass. Non-Go edits skipped
  both gates entirely and passed having been checked by nothing.
- **Why deferred:** a design gap. `evaluateFactoryGate` received
  `leaf.outcome.Edits` -- in-memory post-image bytes -- and had neither the
  repository root nor an execution sandbox.
- **What satisfied it:** `factoryToolchain` materialises the edits as a
  `go build -overlay` overlay against the approved source root and runs
  `go build ./...` and `go test -count=1 ./...` through it, under the same
  discipline as the change-set verification gate: fixed argv, no shell,
  scrubbed offline environment, deadline, process-group kill. The repository is
  never written to; a rejected candidate leaves nothing behind, which has its
  own test.

  Every red case in `factory_toolchain_test.go` is syntactically valid Go, so
  the old gates passed all of them: an undefined symbol, a type error, a
  missing import, a signature its callers no longer match, a passing build
  whose tests fail, and a change set that deletes an embedded asset while
  touching no Go source at all. With no repository root configured both gates
  now fail rather than reporting a pass they did not earn.

- **Two limits, measured rather than assumed, and recorded in the code:**
  - An edit to `go.mod` is not seen. The module is loaded before the overlay
    is consulted, so a candidate that changes requirements is compiled against
    the module as it is on disk. A change set editing `go.mod` is gated on
    everything except the thing it changed.
  - `go build` does not compile test files, so a package left holding only its
    tests builds cleanly. BUILD is therefore not a superset of TEST, and both
    are run.
  - Writing this found a third trap worth naming: overlay keys are matched
    against paths the go command has itself resolved. An unresolved root --
    every temporary directory on darwin is under a symlink -- matches nothing,
    the overlay is silently ignored, and the gate compiles the original tree
    while reporting on the candidate. That is the exact failure being removed,
    reintroduced by the fix; the root is canonicalised, and the first run of
    these tests caught it.


### 2026-08-21 — `product-brain serve` had no root pin

- **Wave:** W3
- **Trigger:** the root pin was added to `sentra-code-memory`'s `serve`, `http`
  and `mcp`. `product-brain serve` is a second JSONL dispatch surface over the
  same `codeserve.Handle` and took no `--root`, so T-004's fix did not reach
  it: a request naming `/` was refused on one surface and answered on the
  other.
- **Why deferred:** it needs the same flag and default treatment, and it landed
  after a branch that had already changed this surface's contract twice.
- **What satisfied it:** the flag, and the resolution moved out of a command's
  main package into `codeserve.ResolveRootFlag` so the two surfaces cannot
  drift apart again -- the first copy of that logic pinned one of them and the
  second surface was reached by nothing. `sentra-code-memory`'s
  `resolveRootPin` is now a five-line adapter over it.
  `product-brain/serve_root_pin_test.go` mirrors the sibling's wiring tests and
  adds `TestRunServeInstallsThePin`, which reads the composition directly: the
  behavioural tests both pass against a `runServe` that resolves a pin and then
  discards it, which is a state this surface could plausibly return to. Red on
  both reverts.

### 2026-08-21 — `stAllStampsMatch` ignored the repository ignore policy

- **Wave:** W3
- **Trigger:** the warm fast path walked with a hardcoded `skipDir` set while
  every other walk uses `repoignore`, so its file census was a strict superset
  of the indexed set and the `len(live) != len(prev.fileStamps)` gate failed
  permanently in any repository with an ignore rule. The README claim was
  corrected in this branch; the code was not.
- **Why deferred:** the fix is small but it changes when the full-refresh path
  runs, which wanted its own measurement rather than riding along with a
  security batch.
- **What satisfied it:** `repoignore` is loaded in `stAllStampsMatch`, so both
  sides of the comparison use one policy. Measured on this repository (1,056
  indexable files, `.pytest_cache/` ignored), second and third opens of an
  unchanged tree:

  | | delta walk | wall |
  | --- | --- | --- |
  | before | 57–61 ms | 209–210 ms |
  | after | not run | 131–146 ms |

  `codecrawl/warm_path_ignore_test.go` covers it. Telling the warm path from
  the delta walk needs care: an unchanged repository reaches the delta walk
  too and also reports `BytesRead == 0` with every file skipped by stamp, so
  that signature does not distinguish them and a first draft passed against
  the unfixed code. Only the warm path returns a zero `Duration`. Red on
  revert, plus a case that a real edit is still detected and one that a new
  ignored file does not defeat the path.


### 2026-08-21 — fifteen fixes had no executable check

- **Wave:** W3
- **Trigger:** a fresh-eyes audit of the triage ledger reverted each fix and
  re-ran the suite. Fifteen rows marked `CONFIRMED` stayed green with their fix
  removed: D-003, D-004, D-006, D-007, D-008, N-004, N-005, N-007, N-008,
  C-009, A-003, A-009, A-010, L-002, L-003. They were relabelled
  `FIXED-UNPROVEN` rather than left claiming evidence they did not have. (This
  entry previously said "sixteen" over a list of fifteen.)
- **Why deferred:** the fixes themselves were correct and had been verified by
  reading; what was missing was the regression guard. Writing fifteen tests
  well is a batch of its own, and writing them badly is how the two fabricated
  red proofs got in.
- **What satisfied it:** one guard per row, each run against its reverted fix
  and observed to fail before being recorded. Writing them found four defects
  the original fixes had missed, each closed alongside its guard:
  - D-007 had tightened the corpus, sidecars and metadata to 0600 and left
    `hotlex.gob` (which carries document text), `gardener.db` and its WAL
    sidecars, and the query log at 0644. The guard is a property of the whole
    brain directory rather than a list of filenames, which is why it found
    them.
  - C-009 recorded a lost receipt in `Output` only when `Output` was empty --
    exactly when the job had succeeded -- so a successful job whose completion
    was never persisted returned `OK=false` with no reason anywhere.
  - `flush` cleared `forceFullFlush` in its corpus branch before
    `flushSidecarsLocked` read the same field, so `DeleteDocuments` never
    rewrote the sidecar base and a deleted document's derived text stayed on
    disk. This was also on the reviewer follow-up list.
  - `TestHNSWFixedCorpusExactVsANNMetrics` asserts a wall-clock p95 that does
    not survive a full-repo `-race` run; the clock is now skipped under the
    race detector rather than the bound being raised.


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
