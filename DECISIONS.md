# Decision ledger

Unresolved decisions surfaced by the work. **A lane cannot close while any
entry is open.**

Distinct from `HARDENING.md`: a deferred check is something we know how to do
later; an open decision is something only a human can settle. Every entry
carries a recommendation — handing over a bare question makes the reader do the
thinking twice.

`- [ ]` open, `- [x]` resolved.

## Settled at intake (2026-08-21)

- [x] **Disposition of dormant subsystems.** Roughly 5,000 lines of privacy,
      isolation and erasure code ships with zero callers. Resolved: make a
      per-package call, each recorded below.
- [x] **Overclaiming surfaces** — factory gates, savings estimates, the
      scmbench baseline, and no-op hooks. Resolved: build the real
      implementations rather than renaming the claims away.
- [x] **Amplification depth.** Resolved: scope to changed code. `-race`
      repo-wide, fuzz on parser, path and digest surfaces, property tests on
      pure functions, mutation limited to touched packages.
- [x] **Delivery.** Resolved: one branch, one commit per batch, single merge to
      `main` at the end.

## Open

None yet. Populated by B11 and B12 as each per-package call is reached.
