# Hardening ledger

Appended whenever a wave defers a check. Deferred is not skipped: this ledger
is what makes deferring safe. Entering W3 means draining it.

Schema per entry: date, wave, trigger, why deferred, what would satisfy it.

## Open

### 2026-08-21 — reranker window not addressed

**Open. Three of the four performance findings are drained below; this is the
one that is not.**

- **Wave:** W3
- **Trigger:** the reranker sends only the first 1,500 bytes of each document,
  which on code is the licence header and imports -- so what is scored is
  frequently not what the document is about.
- **What was done:** the truncation itself was a byte offset, `d[:1500]`,
  which lands mid-rune on any non-ASCII input. The destination is a model
  provider's JSON body, so `encoding/json` substituted U+FFFD and the last
  token of every truncated document was corrupted before it was scored. The
  repository-wide pass that replaced about a dozen of these with `textbound`
  did not reach this one. Fixed and red-proved.
- **Why the window itself is still open:** changing it changes what is scored,
  not how fast it is scored, and the reranker is a credentialed remote service
  (`ZEROENTROPY_API_KEY`). There is no offline lane for it, so a before/after
  quality comparison cannot be run here. Raising the window or chunking around
  candidate definitions without that comparison would be changing retrieval
  quality blind.
- **What would satisfy it:** a quality run against the real reranker with the
  window raised and with chunking around candidate definitions, compared on
  the QA suite's hit@k, not on latency.

## Drained

### 2026-08-21 — `TestFrozenExactly100ChangeFixture` was load-sensitive

- **Wave:** W3
- **Trigger:** failed once during a full `go test ./...` run, passed on two
  subsequent full runs and on seven isolated runs, at this branch and at the
  base revision.
- **Why it was deferred:** unreproducible on demand.
- **The mechanism, now reproduced rather than read.** The original entry
  pointed at `waitForFile`'s 5s deadline, which this test does not call. The
  bound that expires first is `testConfig`'s `CommandTimeout`, which bounds
  every git subprocess a reconcile spawns -- and a reconcile over the frozen
  fixture spawns many.

  `command_timeout_test.go` reproduces it on demand with a git wrapper that
  sleeps a fixed 300ms per invocation, which is what a loaded machine does to a
  subprocess without anything being wrong. Under a constant allowance the
  reconcile fails with `context deadline exceeded` **surfaced from the
  authority**, not from the test framework -- which is exactly why the original
  failure was hard to place: it reads as an ingestion defect rather than as a
  slow subprocess.

- **What satisfied it:** `subprocessAllowance` derives the bound from the test
  binary's own deadline, so the bound follows the budget the runner was given
  instead of a constant chosen on one machine. The same 300ms-slow git is
  absorbed; reverting to a constant makes it fail with the signature above.
  Red-proved.

  One trap, caught by running it: the derived value must be clamped to what
  `ingestion.Config` accepts, which rejects a `CommandTimeout` over ten
  minutes. Without the clamp, `-timeout 20m` produced `ingestion: invalid
  input` on every test in the package -- a worse failure than the one being
  removed.

- **What is still not claimed:** that the *original* 2026-08-21 failure was
  this mechanism. It was never reproduced and cannot be attributed after the
  fact. What is now shown is that the mechanism is real, that its signature
  matches what was observed, and that it no longer exists. If the fixture fails
  again with something other than a git context deadline, this entry was aimed
  at the wrong bound and should be reopened.
- **Verification:** `-count=20 -race` under 12 competing CPU-burning processes
  at `-timeout 20m`: green in 281s.


### 2026-08-21 — three performance findings, measured and fixed

- **Wave:** W3
- **Trigger:** the audit found performance defects confirmed by reading but not
  fixed: `code_exact` re-reads and re-parses the whole repository per query;
  every verb call gob-decodes the entire index under a file lock; HNSW upsert
  is a linear id scan, making load O(n²).
- **Why deferred:** each is a behaviour-preserving optimisation whose risk is a
  retrieval-quality regression, and the branch already carried a large
  correctness diff.
- **What satisfied it.** Measured on this repository, 1,067 indexed files,
  before and after:

  | | before | after |
  | --- | --- | --- |
  | `code_search` | 136 ms | 73 ms |
  | `code_exact` | 384 ms | 146 ms |
  | HNSW load, 40,000 vectors | 1.831 s | 0.809 s |

  - **The index decode** was roughly half the cost of answering a query: 66 ms
    per call, re-reading a file the process had usually just read.
    `loadCached` keys the decoded index on the gob's size and modification
    time, so an external rewrite is picked up rather than served stale, and
    `Save` invalidates so an in-process writer never leaves a reader holding
    the previous index. It is deliberately narrow: only `OpenOrRefresh`'s warm
    read uses it, so every caller that mutates an index still goes through
    `Load` (a private decode) or the force path (a fresh build).

    Sharing one `Index` between concurrent readers is what turned `Graph()`'s
    lazy assignment from a latent race into a real one. It is synchronised
    now; without the mutex the concurrency guard reports five data races.

  - **`code_exact`** cannot early-exit, because its receipt digest covers every
    file's projection digest. What it can avoid is re-parsing content that has
    not changed. `codeindex.Project` is a pure function of (content, limits),
    so the projection is memoised on a hash of exactly those -- not on a file
    stamp, because the input is bytes already in memory and a hash of what is
    being parsed cannot be stale. The receipt is unchanged, which has its own
    test, and an edit changes it, which has another.

  - **HNSW upsert** scanned the id slice to find an existing entry, so the load
    path -- one Upsert per vector -- walked everything inserted before it. An
    id index makes that a lookup.

    The growth rate can only be observed with a clock, and this repository
    already carries an open entry for a wall-clock assertion that fails inside
    a parallel `-race` run. So the measurement is a benchmark, not a test, and
    what is asserted deterministically is the replacement semantics the linear
    scan existed to provide -- including in `Clone`, the second place the id
    index is built, where omitting the rebuild makes the clone start
    duplicating ids.

  The `bench-code` baseline digest is unchanged throughout, so no served answer
  moved.


### 2026-08-21 — savings figures were estimates presented as measurements

- **Wave:** W3
- **Trigger:** `savings` reported `baseline_tokens` / `served_tokens` /
  `saved_tokens` from a flat bytes÷4 heuristic, against a baseline of "an agent
  reads every indexed source file" that no real agent performs, with no
  qualifier on the JSON or the CLI. Separately the ledger had no producer in
  the product: only the benchmark wrote to it, so `savings_summary` answered
  `steps: 0` after any amount of real use.
- **Why deferred:** a real tokenizer means vendoring a BPE vocabulary or
  shipping a model-specific approximation, and an honest baseline means
  measuring what an agent would otherwise have read. Both are design choices.
- **What satisfied it:**
  - Every token field is `*_tokens_est`, and every step and report carries the
    identity of the estimator (`bytes_div_4`) and of the baseline model
    (`whole_tree` or `gold_files`), so a figure produced under one is never
    silently compared with a figure produced under another. The on-disk schema
    went to 2; an older ledger is discarded rather than reinterpreted, being
    derived cache-resident data whose estimator was never recorded.
  - The QA lane reports both baselines. **Measured on the checked-in fixture:
    0.50 against the whole tree, 0.42 against the gold files** -- the strawman
    was worth eight points of claimed saving. The headline figure stays on the
    whole-tree baseline so the threshold and the digest keep their meaning, but
    it is named, and the honest one sits beside it in the artifact.
  - `code_search` now records a step per served response, so the ledger has a
    producer in the product. Its baseline is the set of files the answer cites
    -- what the caller would otherwise have read to reach the same hits.
  - A durable rewrite of the ledger measures 4.2ms at capacity while the
    retrieval path answers in well under a millisecond, so writing per response
    would cost several times what it measures. Steps are queued and written
    every 32, and a summary flushes before reading. `bench-code` p95 and the
    baseline digest are both unchanged.

  Two mistakes in the first version, each caught by a test rather than by
  reading: the queued steps were held in a `savings.Ledger` opened per request
  and thrown away, so nothing was ever written; and caching that Ledger instead
  made a long-lived reader answer from a snapshot taken before other writers'
  records. Only the queue is process state now, and the file stays the source
  of truth.

- **Not done, and still true:** the estimator is not any model's tokenizer, and
  says so in its name rather than being corrected. A real one is the dependency
  decision this entry originally described.


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
