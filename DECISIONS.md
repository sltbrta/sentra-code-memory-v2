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

## Per-package dispositions (2026-08-21)

The audit reported "roughly 5,000 lines of safety machinery with zero callers".
Checking each package rather than accepting the figure, three of those claims
were wrong:

- `tenant.AuthorizeBrainPath` — **falsified.** Called three times from
  `product-brain` (`cmd_company.go:382`, `:392`, `cmd_platform.go:165`). It is
  the wired cross-tenant path guard, not dead code.
- `federation` — **falsified.** Imported by `brain/cmd/product-brain`.
- `chunking` — confirmed eval-only, imported by `chunk-eval` alone.

Calls taken, each reversible by reverting the named commit:

- [x] **`workflowinspect` — deleted.** 628 lines, genuinely zero importers. Its
      "authorization" was a hardcoded compare against the principals `operator`,
      `alice` and `inspector`, its registry was three fixture nodes, and its
      cost model returned the constant 0.01. Shipping that as an
      authorization-and-integrity boundary is worse than shipping nothing,
      because it reads as a control. Restore with `git revert` if a real
      registry and policy source arrive.
- [x] **`contentprivacy` — kept and repaired, not yet wired.** Its detector
      missed every vendor key format in common use (OpenAI `sk-`, GitHub
      `ghp_`/`github_pat_`, Slack `xox*-`, Google `AIza`, GitLab, Hugging Face)
      and could not match a private key whose END marker had been cut off by
      chunking — the case that matters most. Those are fixed and tested.
      Wiring it into the ingest path is a behavioural change with latency and
      retrieval-quality consequences; see the open decision below.

## Open

- [ ] **Wire `contentprivacy` into the hosted ingest path?**
      *Why it matters:* it is the only PII and secret redaction in the product
      and it currently runs nowhere, so emails, SSNs, cards, bearer tokens and
      private keys in ingested content are written verbatim to `chunks.jsonl`,
      the HotLex index and the dense store. The `guard.go` documentation
      describes guarantees nothing provides.
      *Options:* (a) route `hosted` ingestion through
      `ProductionProjectionAdapter.AdmitAndPublish` so cache and index text can
      only be constructed by `Guard.inspect`; (b) leave it dormant and mark the
      package non-shipping in its own docs; (c) delete it.
      *Recommendation:* (a). The detector is now correct, and the alternative is
      shipping a redaction story that does not redact. It needs its own
      before-and-after retrieval-quality run, because redaction changes indexed
      text and therefore ranking — which is why it is not folded into this pass.

- [ ] **Persist `contentprivacy` tombstones and receipts?**
      *Why it matters:* `Tombstone()` is documented as "the retained
      non-content authority blocking resurrection", and it is a Go map. A
      restart drops every tombstone, so deleted content can be re-ingested, and
      the append-only receipt log vanishes with it.
      *Options:* back them with the existing `localstate` authority tables, or
      document the guarantee as process-lifetime only.
      *Recommendation:* back them with `localstate`, together with (a) above —
      an erasure guarantee that does not survive a restart is not one.

- [x] **Wire `orgscope.Erase`, or fan out in `deletion`? — fan-out in
      `deletion` (2026-08-21).**
      *What was wrong:* `internal/deletion` flipped a manifest to
      immediate-deny and scheduled a purge job for the object store. Nothing
      removed the projections a query is answered from, so a deleted document
      stayed in the corpus, the lexical index, the memory cortex and the query
      log, and went on being retrieved, ranked and cited.
      *What was built:* `deletion.Purge`, a fan-out over one small port per
      substrate, and the substrate implementations that did not exist. The
      cortex had no deletion at all -- every mutator added and nothing removed
      -- so `PurgeDocuments` now covers all eleven document-keyed projections
      including both directions of the adjacency, and the query log, which
      records the document ids each question was answered from, is rewritten
      through `durablefile`.
      *What was reused from `orgscope`:* its discipline, not its code. It
      erases its own in-memory model rather than the product's substrates.
      What carries over is naming the exact coverage, counting what each
      substrate removed, and then **looking again** -- verification is a second
      pass, because a delete count says how many entries a loop matched, not
      whether the document survives somewhere the loop did not look. A test
      covers exactly that: a substrate that reports a delete and keeps the
      document is caught.
      *What is deliberately not done:* the dense backends. There are five
      behind `denseBackend` -- SQLite, Postgres, FAISS, HNSW, Qdrant -- and
      none exposes a delete. Adding one to each without being able to exercise
      Postgres or Qdrant would ship an erasure path that has never run. The
      receipt names `vectors` as **skipped** and refuses `VerifiedComplete`,
      so the gap is in the artifact rather than in a footnote. Closing it is
      the remaining work, and is now a bounded task: implement one method per
      backend against a port that already exists and is already tested.
      *Also:* `internal/deletion` had no test file at all, which is part of why
      none of this was noticed. It has one now.

- [x] **Delete the Rust `workers/code-index` crate? — yes, deleted
      (2026-08-21).** Its own doc comment said it "does not index code, invoke
      a compiler, read a repository, access a network, or claim compatibility
      with the canonical contracts"; `main` read stdin and printed two digests.
      Nothing called it. Removed along with the `cargo` line in `check-all`,
      the CI `cargo` job (and with it the Rust toolchain requirement), and the
      references in the README, the architecture doc and the installation
      prerequisites.

      One correction to the entry as written: it said the crate was "referenced
      by the justfile, CI and 95 Bazel files". 94 is the repository's *total*
      Bazel file count, and exactly one of them -- the crate's own
      `BUILD.bazel` -- referenced it. The deletion was a package removal after
      all, not a repo-shape change. `just check-all` passes without it.

      Reversible with `git revert`. The Rust itself was the cleanest code in
      the repository -- no unsafe, no unwrap, zero third-party crates -- so
      this was about wiring, not quality.

- [ ] **Wire or remove `llmadapter`?**
      *Why it matters:* 727 lines with no non-test importer, including the
      Gemini provider seam. Its claim-extraction prompt also concatenates
      repository content directly into the instruction with no delimiter or
      untrusted-content framing.
      *Options:* wire it behind an explicit opt-in, or remove it until a
      consumer exists.
      *Recommendation:* keep it dormant for now and fix the prompt framing
      before it is ever wired — but say so in its package doc, which currently
      reads as though it is in use.
