# Session handover — hardening pass, 2026-08-21

Read this first, then `HARDENING.md` and `DECISIONS.md`. The evidence table is
`docs/findings/2026-08-21-audit-166-triage.md`.

## Where things stand

- `main` is **pushed**. 44 commits ahead of the branch base `9278cdf`.
- Working tree clean. `just check`, `just check-race`, `just check-all`,
  `just bench-code` and `govulncheck` all pass; `govulncheck` reports 0.
- The `bench-code` baseline digest is unchanged across every commit in this
  pass, so nothing here moved a served answer.
- **`DECISIONS.md` has no open entries.** The lane can close.
- `HARDENING.md` has **two** open entries. Both are blocked on something this
  session could not obtain, not on effort, and both are described below.

## What is still open

### 1. The reranker window — blocked on credentials

The reranker sends the first 1,500 bytes of each document, which on code is
the licence header and imports. Raising it, or chunking around candidate
definitions, changes **what is scored** rather than how fast, and the reranker
is a credentialed remote service (`ZEROENTROPY_API_KEY`) with no offline lane.
The before/after quality comparison cannot be run here, and doing it blind
would be changing retrieval quality without measuring it.

The truncation itself is fixed: `d[:1500]` is a byte offset that lands
mid-rune, and the destination is a JSON body, so `encoding/json` substituted
U+FFFD and corrupted the last token of every truncated document before it was
scored.

**To close it:** a quality run against the real reranker with the window raised
and with chunking around candidate definitions, compared on the QA suite's
hit@k rather than on latency.

### 2. `TestFrozenExactly100ChangeFixture` — never reproduced

**The mechanism it would have used no longer exists, but the original failure
was never reproduced, so nothing is proved to have fixed it.**

`subprocessAllowance` now derives the git-subprocess bound from the test
binary's own deadline, so the framework always reports first: a stuck git fails
named and with a stack, a merely slow one does not. Verified at `-count=20
-race` under 12 competing CPU-burning processes with `-timeout 20m`, green in
281s — and green with the *old* flat allowances too, which is exactly why the
entry stays open.

**To close it:** a reproduction. Failing that, capture the next occurrence with
`-race`: a `context deadline exceeded` from a git command confirms the
diagnosis; anything else means this work was aimed at the wrong bound.

### 3. Ledger coverage — still 83 rows against 166 candidates

The B1/B10 candidate list **was never committed to this repository**, so those
rows cannot be recovered. Reconstructing them means re-running the review that
produced them.

What exists instead is a **fresh sweep** of the areas the previous handover
named, recorded in the triage table under its own heading and labelled as a
sweep rather than a reconstruction. Three classes came back clean —
tie-unstable ranking, fail-open authorization, panic on caller-supplied bounds
— and two did not; both are fixed and rowed as S-001 and S-002.

## What this pass did

Every fix was run against its own reverted state and observed to fail before
being recorded. Where a change could **not** be red-proved, that is stated
rather than glossed — there are four such places, and each says so in the
ledger and in the code.

**The fifteen unproven fixes now have guards.** Writing them found four defects
the original fixes had missed, including three files still published 0644 after
a pass that was supposed to have tightened them, and a `forceFullFlush` flag
cleared before the sidecar writer read it.

**Five `HARDENING.md` entries drained.** The factory BUILD and TEST gates now
build and test through a `go build -overlay` — every red case is syntactically
valid Go, so the old gates passed all of them. Savings figures are named as
estimates, with both baselines reported: 0.50 against the whole tree, 0.42
against the gold files. Performance measured and fixed: `code_search` 136ms →
73ms, `code_exact` 384ms → 146ms, HNSW load of 40k vectors 1.83s → 0.81s.

**All six reviewer follow-ups closed**, and then the last one properly: the
pin's check/use gap is closed with `os.Root`, so resolution and open are one
operation. Measuring it corrected the claim — the window is not a symlink swap
(0 escapes in 43,128 reads) but a *real directory* component replaced between
resolve and open, which is deterministic.

**All five decisions settled and implemented.** Redaction wired with a measured
quality run (hit@1 and hit@3 unchanged at 1.00) and durable tombstones.
Deletion actually deletes, now including the dense vectors — the HNSW index had
no deletion at all, which was the real blocker. The dormant Rust crate is gone.
`llmadapter` is wired behind an off-by-default opt-in, after its prompt framing
was fixed.

## Conventions worth knowing before you touch anything

- Commit messages: no `Co-Authored-By` trailer — the repo hook bans attribution
  trailers (its workflow rule 10).
- The pre-commit hook runs gitleaks, actionlint, markdownlint and **goimports**
  (not just gofmt). `just check` checks the same things.
- Markdown is linted at 80 columns with tables excluded.
- Red proof is required from W2 up. A test that has never been red proves
  nothing — and a test that passes both before and after a change is
  characterisation, not proof. Three tests written this pass were reclassified
  or deleted on that basis.
- **A wall-clock assertion inside `go test -race ./...` measures the machine,
  not the code.** It bit four times this pass. Where a property can only be
  observed with a clock, it belongs in a benchmark; where a bound only exists
  to catch a hang, derive it from `t.Deadline()` rather than guessing a
  constant.
- **Check a fixture actually discriminates.** A limit divisible by the rune
  width cuts on a boundary and proves nothing; a corpus of plain English words
  yields no tokens from an extractor that requires a digit. Both wasted a cycle
  here.
- The Rust toolchain is no longer a prerequisite: `check-all` and CI dropped the
  `cargo` lane with the crate.
