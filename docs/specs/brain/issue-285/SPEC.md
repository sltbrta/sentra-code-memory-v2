# Issue #285 — Share retrieval hydration with answer-time whole-document hydration

## Problem

Retrieval hydrates selected chunk IDs and bounded sibling windows before
`AnswerOpts` runs. Answer-time whole-document hydration can therefore repeat
work that the same request already completed.

## Contract

Answer hydration preserves the existing `WholeDocN`, `WholeDocChunks`, and
`WholeDocBudget` bounds, document order, chunk de-duplication, citation/source
metadata, and richer-text upgrade behavior.

Before answer hydration mutates the pool, it records:

- **reused**: passages with non-empty text and a chunk ID;
- **skipped**: passages without a chunk ID, which cannot use Neon chunk-ID
  hydration;
- **needs fetch**: unique chunk IDs for which no duplicate passage has usable
  text.

Missing chunk IDs are fetched through the existing BrainID-scoped
`hydrateByChunkIDs` query. Whitespace-only text is insufficient and may be
replaced by returned text. A duplicate empty passage does not trigger a fetch
when another passage with the same chunk ID already has usable text.

For each selected document, sibling hydration does one of two things:

1. If the pool contains at least `WholeDocChunks` distinct, non-empty chunks
   marked by an earlier ordinary sibling hydrate, reuse that complete bounded
   window without another sibling query.
2. Otherwise, query the original top-`WholeDocChunks` sibling window. Do not
   exclude present chunk IDs in SQL. Present hits retain richer-text upgrade
   behavior; new hits append in query order. At most `WholeDocChunks` returned
   hits are processed per document, even if an alternate store or test driver
   returns too many rows.

Date-prioritized hydration always queries its original date-ordered window,
because an ordinary hydrated window does not prove coverage of that ordering.

This prevents an exclusion plus unchanged `LIMIT` from fetching a second page
or increasing pack cardinality.

## Diagnostics

All counters are stamped under `AnswerResult.RetrievalDiagnostics`:

| key | meaning |
| --- | --- |
| `answer_hydrate_reused_n` | pre-hydrate passages whose existing chunk text was reused |
| `answer_hydrate_skipped_n` | pre-hydrate passages without a chunk ID |
| `answer_hydrate_fetched_n` | requested missing chunk IDs returned with usable text |
| `answer_hydrate_sibling_reused_n` | original-window sibling hits already present, including a fully covered window skipped without a query |
| `answer_hydrate_sibling_fetched_n` | original-window sibling hits newly appended |
| `answer_hydrate_skip_reason` | `lean_tier` or `whole_doc_not_requested` when whole-document hydration did not run |

The reused/skipped counters are always captured before hydration; a final pool
containing newly hydrated passages is never reclassified as pre-hydrate reuse.
Existing keys (`answer_full_doc_hydrate`, `answer_whole_doc_smf`,
`answer_full_doc_n`, `answer_full_doc_chunks`, and
`answer_hydrate_by_id_n`) remain available.

## Authorization

Both `hydrateByChunkIDs` and sibling queries remain scoped by
`cfg.BrainID`. Reuse only avoids a repeated query within the already-authorized
retrieval pool; it does not broaden the authorization boundary. Citation and
source metadata on existing passages remain authoritative during text upgrades.

## Out of scope

- Stage parallelization (#276).
- Retrieval ranking or scoring changes.
- Modal URL changes.
- Benchmark-specific routing.

## Verification

Focused tests cover missing and whitespace-only text, duplicate chunk IDs,
BrainID query scope, citation metadata, richer-text upgrades, deterministic
order, explicit pre-hydrate diagnostics, full-window query skipping, and a
regression guard against page-two append or per-document sibling-budget growth.
