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

- [x] **Wire `contentprivacy` into the ingest path? — yes, wired
      (2026-08-21).** It is the only PII and secret redaction in the product
      and it ran nowhere, so emails, national identifiers, card numbers, bearer
      tokens and private keys were written verbatim to `chunks.jsonl`, the
      HotLex index, the dense store and the memory cortex while `guard.go`
      documented guarantees nothing provided.

      Publication goes through `ProductionProjectionAdapter` rather than
      calling `Guard.Admit` and using the result, so redacted text can only
      have come from the validated admission path -- which is what that type
      exists for.

      **Redaction happens at the document boundary, above
      `DocumentsToChunks`.** Two reasons, the second decisive: a secret split
      across a chunk boundary is invisible to a per-chunk detector (the
      private-key rule already had to be widened because chunking cut the END
      marker off), and `BurstIngestLocal` fans the *documents* out to
      `seedMemoryAfterIngest` and `seedDenseAfterIngest`, not the chunks.
      Redacting only the chunks would have sanitised the corpus and left the
      raw text in the cortex and the vector store -- which reads as redaction
      and is not.

      A withheld document is reported by id with its disposition rather than
      silently dropped, and a client with no guard behaves exactly as before,
      so redaction is a composition choice rather than something acquired by
      accident.

      **The quality run this decision required.** Redaction changes indexed
      text and therefore ranking, so the trade was measured rather than
      assumed, and the measurement is kept as a test rather than performed
      once. Over an 18-document corpus of which 3 carry sensitive spans:

      | | hit@1 | hit@3 |
      | --- | --- | --- |
      | unguarded | 1.00 | 1.00 |
      | guarded | 1.00 | 1.00 |

      Redaction removes the sensitive span and leaves the surrounding text, so
      the documents stay findable by everything that made them findable before.
      The guard fails if the guarded run redacts nothing, so the comparison
      cannot pass by being vacuous.

- [x] **Persist `contentprivacy` tombstones and receipts? — yes
      (2026-08-21).** `Tombstone()` is documented as "the retained non-content
      authority blocking resurrection" and was a Go map: a restart dropped
      every tombstone, so erased content could be re-ingested the moment the
      process came back, and the append-only receipt log vanished with it.

      `Guard` gains a `StateStore` port and `NewWithState`. The package stays
      persistence-neutral -- a deliberate property -- but a deployment with
      durable storage can now be held to the guarantee. `MemoryStateStore`
      keeps the old behaviour and names it, so process-lifetime-only is a
      choice a composition makes rather than a silent default.

      **Deviation from the recommendation, stated rather than quietly taken:**
      the entry said to back them with `localstate`. `localstate` is the
      migration-driven SQL authority store used by the hosted and gateway
      paths; the brain this is wired into is a filesystem projection
      (`meta.json`, `chunks.jsonl`, `gardener.db`), and pulling a SQL authority
      database into it to hold two append-only logs would be the larger change,
      not the smaller one. `FileStateStore` writes two fsync-per-record JSONL
      logs at 0600 beside the rest of the brain. The port is what the decision
      was really about: a `localstate`-backed implementation for the hosted
      substrate is now a struct with three methods, and nothing else moves.

      Append-only rather than a rewritten snapshot because the failure that
      matters is losing a tombstone: an interrupted rewrite can lose records
      that were already durable, while an append that does not survive simply
      is not there. A tombstone whose durable write fails denies rather than
      being kept only in memory.

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
      *The dense backends, closed 2026-08-21.* `denseBackend` gained the purge
      port and all five implement it. Three are exercised here -- the
      in-memory store, the HNSW index, and the SQLite-backed local projection
      -- and Postgres implements the same SQL as its writer. With a dense
      backend configured, a purge now reaches the vectors and the receipt
      reports `VerifiedComplete`, which it could not do before.

      The HNSW index had no deletion at all, which was the real blocker: it
      backs the local default. Removal compacts the parallel slices and
      rewires rather than tombstoning a slot, because "the vector is no longer
      here" has to mean the bytes are gone. The graph is rebuilt rather than
      patched -- every neighbour index refers to a slot that has moved, and a
      stale index still points at a live slot, so nothing crashes and the
      results are simply wrong.

      **FAISS and Qdrant, closed 2026-08-21.** They were left returning
      `ErrPurgeUnsupported` on the grounds that shipping an erasure path this
      repository cannot exercise is worse than a named gap. That reasoning
      missed the shape of the fan-out: it verifies by **re-querying** after the
      delete, so a wrong endpoint returns non-2xx, which becomes an error,
      which the verification reports as a residual. A wrong implementation
      therefore surfaces as an *incomplete* erasure rather than as a successful
      one -- and refusing to try left those deployments permanently unable to
      erase, which is strictly worse than trying against a self-checking
      fan-out.

      Both are now implemented against their documented APIs and exercised
      against fakes that speak them: Qdrant deletes by a `dsid` payload filter
      and verifies with an exact count; the FAISS sidecar uses the same
      request shape as its documented upsert and search. A test drives both
      against a server that answers 404 to everything, and asserts the purge
      is reported as a failure rather than a success.

      *Still not claimed:* that either has run against a live server. The
      verification pass is what makes that tolerable rather than reckless.

      A backend that cannot be reached at all is not wired as a purger, and the
      receipt names `vectors` as skipped. The reachability probe checks
      configuration before its empty-input short-circuit -- without that, an
      unconfigured backend answered "nothing to do", got wired, and would have
      reported zero removals as an erasure. Caught by its own test.
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

- [x] **Wire or remove `llmadapter`? — wired behind an explicit opt-in
      (2026-08-21).** 727 lines with no non-test importer: a second
      implementation of query expansion and candidate scoring, including a
      Gemini provider seam, sitting beside the one in `llm_multiquery.go` that
      is actually used.

      **The prompt framing was fixed first, not after.** Every prompt in the
      package concatenated content the caller did not author straight into the
      instruction -- a document's text after `ExtractClaims`' directives, a
      user's query after `ExpandQuery`'s, retrieved passages after
      `ScoreCandidates`' -- so a document containing "ignore the above and ..."
      was structurally indistinguishable from the operator speaking. Wiring a
      consumer to that would have taken a dormant defect and made it
      reachable. Content is now fenced in a per-call randomised block that it
      cannot close, and the system prompt states the fenced region is data.
      Framing is not immunity and the code says so; what it removes is the part
      that was the caller's fault.

      *The opt-in:* `OUROBOROS_BRAIN_LLMADAPTER_EXPAND=1`, off by default,
      consuming `ExpandQuery` in the retrieval path. Off, nothing changes.
      On and unconfigured, llmadapter's own deterministic fallback answers --
      it abstains rather than fabricating -- so an absent key degrades to
      expansions rather than to an error, which is what makes an opt-in over an
      unexercised path safe to turn on. Whatever it returns is added to the
      deterministic variants, never substituted for them, so enabling it cannot
      narrow retrieval.

      *Still without a consumer:* `ScoreCandidates` and `ExtractClaims`. The
      package doc now says which of the three is wired and which are not,
      instead of describing all of them in the present tense.
