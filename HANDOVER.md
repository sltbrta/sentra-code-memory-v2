# Session handover — hardening pass, 2026-08-21

Read this first, then `HARDENING.md` and `DECISIONS.md`. The evidence table is
`docs/findings/2026-08-21-audit-166-triage.md`.

## Where things stand

- `main` is **pushed**. 50 commits ahead of the branch base `9278cdf`.
- Working tree clean. `just check`, `just check-race`, `just check-all`,
  `just bench-code` and `govulncheck` all pass; `govulncheck` reports 0.
- The `bench-code` baseline digest is unchanged across every commit in this
  pass, so nothing here moved a served answer.
- **`DECISIONS.md` has no open entries.** The lane can close.
- `HARDENING.md` has **two** open entries. Both are blocked on something this
  session could not obtain, not on effort, and both are described below.

## What is still open

**Nothing.** `HARDENING.md` and `DECISIONS.md` both have empty Open sections.

Three things are **narrowly not claimed**, each stated inside the entry that
claims the rest rather than held open as a whole:

1. That the new reranker window improves `zerank-2`. The remote lane cannot be
   exercised here. What is measured is that the head-window *policy* loses the
   answer on a reranker whose scoring can be inspected. Confirming the remote
   effect needs a credentialed QA run comparing hit@k.
2. That the original 2026-08-21 `TestFrozenExactly100ChangeFixture` failure was
   the command-timeout mechanism. It was never reproduced and cannot be
   attributed after the fact. The mechanism now reproduces on demand and no
   longer exists; if the fixture fails again with anything other than a git
   context deadline, that entry was aimed at the wrong bound.
3. That the FAISS and Qdrant purges have run against a live server. They are
   implemented against documented APIs and exercised against fakes. That is
   tolerable only because the fan-out verifies by re-querying: a wrong endpoint
   returns non-2xx, becomes an error, and is reported as a residual -- so a
   wrong implementation surfaces as an incomplete erasure, never a successful
   one.

Each was previously an open entry justified by "cannot be measured here". Two
of those justifications turned out to be false on inspection, which is the main
lesson of the last pass: the reranker *does* have an offline lane
(`rerank.LexicalReranker`), and the type-aware truncation lint *does not* need
a new dependency (`go/importer` is standard library).

## Ledger coverage

Still 83 original rows against 166 candidates: the B1/B10 candidate list was
never committed, so those rows cannot be recovered, and reconstructing them
means re-running the review that produced them.

What exists instead is a **fresh sweep** of the areas the previous handover
named, recorded in the triage table under its own heading and labelled as a
sweep. Three classes came back clean; three did not, and are rowed as S-001,
S-002 and S-003.

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
- **When an entry says "cannot be measured here", check that.** Two such
  justifications in this ledger were false: the reranker has an offline lane
  (`rerank.LexicalReranker`), and the truncation lint needs no new dependency
  (`go/importer` is standard library). Both entries were closed by testing the
  excuse rather than the code.
- **Prove the small claims too.** "Fixed for consistency, no observable
  change" was recorded once. Writing a differential fuzz test to prove it took
  46 seconds to disprove it, and the reason was a P1 defect in
  `textbound.Bytes` that emptied every non-UTF-8 input at twenty call sites.
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
