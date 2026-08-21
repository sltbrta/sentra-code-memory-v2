# Session handover — hardening pass, 2026-08-21

Read this first, then `HARDENING.md` and `DECISIONS.md`. The evidence table is
`docs/findings/2026-08-21-audit-166-triage.md`.

## Where things stand

- `main` is **pushed**. 38 commits ahead of the branch base `9278cdf`.
- Working tree clean. `just check`, `just check-race`, `just check-all`,
  `just bench-code` and `govulncheck` all pass.
- The `bench-code` baseline digest is unchanged across every commit in this
  pass, so nothing here moved a served answer.
- **`DECISIONS.md` has no open entries.** The lane can close.
- `HARDENING.md` has **two** open entries, both blocked on something this
  session could not obtain rather than on effort. They are the first thing
  below.

The previous handover's push blocker is gone: the detector's synthetic vendor
fixtures are assembled from split prefixes at run time, so no scannable literal
exists in the source or in history, and the local gitleaks allowlist that only
ever covered the pre-commit hook is gone with it.

## What is still open

### 1. The reranker window (`HARDENING.md`)

The reranker sends the first 1,500 bytes of each document, which on code is the
licence header and imports. Raising it, or chunking around candidate
definitions, changes **what is scored** rather than how fast — and the reranker
is a credentialed remote service (`ZEROENTROPY_API_KEY`) with no offline lane,
so the before/after quality comparison cannot be run here. Doing it blind would
be changing retrieval quality without measuring it.

What was fixed is the truncation itself: `d[:1500]` is a byte offset that lands
mid-rune, and the destination is a JSON body, so `encoding/json` substituted
U+FFFD and corrupted the last token of every truncated document before it was
scored. The repository-wide pass that replaced a dozen of these with
`textbound` had missed this one.

### 2. `TestFrozenExactly100ChangeFixture` (`HARDENING.md`)

**Not reproduced, so nothing about it is proved.** The original entry pointed
at `waitForFile`'s 5s deadline, which this test does not call. The bound that
expires first is `testConfig`'s `CommandTimeout: 10s`, which bounds every git
subprocess a reconcile spawns — so the failure would surface as a context
deadline inside production code rather than as an obviously test-shaped
timeout. Both allowances now scale under the race detector.

The entry stays open because an attempt to reproduce failed: the package ran at
`-count=20 -race` under 12 competing CPU-burning processes, once with the
scaled allowances and once with the original flat ones, and both passed. That
makes the change defensive, not proven. The entry records where to look when it
next occurs.

### 3. Dense-store erasure (`DECISIONS.md`, recorded as done-with-a-gap)

`deletion.Purge` reaches the corpus, the lexical index, the memory cortex and
the query log. It does **not** reach the dense vectors: there are five
implementations behind `denseBackend` — SQLite, Postgres, FAISS, HNSW, Qdrant —
and none exposes a delete. Adding one to each without being able to exercise
Postgres or Qdrant would ship an erasure path that has never run.

The receipt names `vectors` as skipped and refuses `VerifiedComplete`, so a
purge that reached three of four substrates cannot read as a deletion. Closing
this is now bounded: one method per backend against a port that exists and is
tested.

### 4. Ledger coverage is still partial — be honest about this

`docs/findings/2026-08-21-audit-166-triage.md` holds **83 rows against 166
candidates**. Batches B1 and B10 — mostly P2/P3 findings in `hosted`,
`query`/`rerank`, `gateway` and `broker` — were never given rows. They are not
fixed and not falsified: they are untriaged, and **the candidate list itself
was never committed to this repository**, so they cannot be recovered from
here. Reconstructing them means re-running the review that produced them.

The section heading previously read "Populated during triage. Each row carries
the red-proof command and its output." over an empty section. It now says what
is true.

## What this pass did

Every fix below was run against its own reverted state and observed to fail
before being recorded. That discipline is the point: two red proofs in the
previous branch had been written, recorded as evidence, and never actually run.

**The fifteen unproven fixes now have guards.** A fresh-eyes review had
reverted each one and found the suite still green. Writing the guards turned up
four defects the original fixes had missed:

- D-007 tightened the corpus, sidecars and metadata to 0600 and left
  `hotlex.gob` (which carries document text), `gardener.db` and its WAL, and
  the query log at 0644. The guard is a property of the whole brain directory
  rather than a list of filenames, which is why it found them.
- C-009 recorded a lost receipt in `Output` only when `Output` was empty —
  exactly when the job had succeeded — so a successful job whose completion was
  never persisted returned `OK=false` with no reason anywhere.
- `flush` cleared `forceFullFlush` before the sidecar writer read it, so
  `DeleteDocuments` never rewrote the sidecar base and a deleted document's
  derived text stayed on disk.
- A wall-clock ANN assertion that does not survive a parallel `-race` run.

**Five `HARDENING.md` entries drained.** The factory BUILD and TEST gates now
build and test, through a `go build -overlay` against the real module: every
red case is syntactically valid Go, so the old gates passed all of them.
`product-brain serve` is pinned. The warm index path is reachable again (it was
structurally unreachable in any repository with an ignore rule; 210ms → 138ms
here). Savings figures are named as estimates and the ledger has a producer.
Three of four performance findings are fixed with measurements: `code_search`
136ms → 73ms, `code_exact` 384ms → 146ms, HNSW load of 40k vectors 1.83s →
0.81s.

**All six reviewer follow-ups closed**, including a `durablefile` leak of one
fd and one temp file per panicking write, and `?operator_trust=1` — which
granted operator trust over the query string, contradicting the README, the ops
runbook and the refusal message itself.

**All five decisions settled and implemented.** Redaction is wired into ingest
with a measured quality run (hit@1 and hit@3 unchanged at 1.00) and durable
tombstones. Deletion actually deletes. The dormant Rust crate is gone.
`llmadapter` is wired behind an off-by-default opt-in, after its prompt
framing was fixed — not after wiring it.

## Conventions worth knowing before you touch anything

- Commit messages: no `Co-Authored-By` trailer — the repo hook bans attribution
  trailers (its workflow rule 10).
- The pre-commit hook runs gitleaks, actionlint, markdownlint and **goimports**
  (not just gofmt). `just check` checks the same things.
- Markdown is linted at 80 columns with tables excluded; delimiter rows need
  `| --- |` spacing.
- Red proof is required from W2 up. A test that has never been red proves
  nothing.
- **A wall-clock assertion inside `go test -race ./...` measures the machine,
  not the code.** Three separate ones bit this pass. Where a property can only
  be observed with a clock, it belongs in a benchmark, and the deterministic
  half belongs in the test.
- The Rust toolchain is no longer a prerequisite: `check-all` and CI dropped the
  `cargo` lane with the crate.
