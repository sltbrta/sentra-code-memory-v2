# HotLex mmap snapshot

**Status:** implemented for issue #300
**Format:** `HOTLEX2`, version 2

HotLex serving images are bounded, checksummed binary files whose corpus tables
are queried directly from a read-only memory mapping. The historical
`hotlex.gob` filename and `SaveGob`/`LoadHotLexGob` APIs remain for deployment
compatibility. `SaveGob` writes `HOTLEX2`; the deliberately named
`SaveLegacyGob` and CLI `--format legacy-gob` are rollback-only escape hatches.

## Serving contract

- A current load allocates only the small mapping descriptor, brain/generation
  strings, and integrity-check state. It does not decode document, term,
  posting, or string tables onto the Go heap.
- The loader verifies the SHA-256 digest, exact file/section sizes, hard count
  limits, UTF-8/string bounds, sorted unique document and term keys, posting
  ranges, document statistics, and posting order before the index is usable.
- `LoadHotLexSnapshot` can bind the image to an expected `BrainID` and
  `Generation`. Wrong-brain returns `ErrHotLexScope`; a missing or different
  expected generation returns `ErrHotLexStale`. Hosted serving always binds the
  brain and optionally binds `OUROBOROS_ERB_HOTLEX_GENERATION`; local serving
  binds both brain and current durable generation.
- Corrupt, truncated, stale, and cross-brain images never become the client's
  active HotLex. Hosted mode retains its existing bounded missing-HotLex
  fallback. Local mode retains its authoritative chunk-store rebuild path.
- Search uses the same BM25 parameters and chunk-id tie-break as the mutable
  index. The canonical writer sorts documents, terms, and postings, so identical
  logical indexes publish identical bytes and deterministic rankings.
- Both paths bound result selection with a size-`limit` min-heap under the same
  strict total order (score desc, chunk-id asc) instead of sorting every
  matched document. Chunk ids are unique in any validated index, so the order
  is total and the bounded top-k ranking is identical to the historical full
  sort; tests compare both paths against a full-sort reference at corpus scale.
- High-document-frequency pruning is explicit and opt-in only
  (`SearchWithOptions` + `HotLexSearchOptions.MaxDocumentFrequency`; the zero
  value is legacy semantics). Identifier-bearing terms (digits or `_`, e.g.
  error codes, versions, code symbols) are never pruned. Every decision is
  observable through `HotLexSearchStats` (matched-doc count, pruned terms,
  protected identifier terms), and the mapped and mutable paths report
  identical stats.
- `Client.Close` releases the mapping. A finalizer is only a leak safety net for
  direct loader callers.

The integrity pass is linear in file bytes and can fault mapped pages into the
OS page cache; “zero decode” means zero corpus object-graph decoding/heap
retention, not zero I/O. Query-time term lookup is binary search over the mapped
term table followed by direct posting/doc record reads.

## Layout and bounds

All integers are fixed-width little-endian. Offsets are relative to either the
file or the string section as specified by the header.

| Section | Record | Purpose |
| --- | ---: | --- |
| Header | 256 bytes | magic/version, exact file size, section offsets/counts, brain/generation offsets, `AvgDL`, `sumLen`, SHA-256 |
| Documents | 64 bytes | string offsets/lengths for chunk id, document id, source URI, optional text; token length and text flag |
| Terms | 32 bytes | term string offset/length and contiguous posting range |
| Postings | 8 bytes | `uint32` document ordinal and term frequency |
| Strings | variable | brain, generation, document metadata/text, then terms |

Hard loader/writer limits are 8 GiB per image, 5,000,000 documents, 20,000,000
terms, 250,000,000 postings, and 3 GiB of strings. Scope fields are capped at
4 KiB. Counts, multiplication, addition, and host address-space conversion are
checked before mapping sections or allocating recovery objects.

## Publication and recovery

`SaveGob` writes a same-directory uniquely named candidate, flushes it, writes
the final digest-bearing header, `fsync`s the file, atomically renames it over
the target, then `fsync`s the parent directory. Candidate validation happens
before target mutation; failed publication leaves the last good image and no
fixed-name partial file.

Migration never silently destroys the only gob-only recovery image. When the
target contains a validated legacy gob, `SaveGob` first preserves its exact
bytes atomically at `<target>.rollback.gob`; an invalid or cross-brain legacy
source rejects migration instead of being overwritten. For a fresh projection,
`SaveGobWithRollback` and CLI `--rollback-gob <path>` explicitly dual-write a
legacy rollback image before HOTLEX2. `SnapshotFormat` and hosted/CLI logs
report
the successfully validated `hotlex2` or `legacy-gob` format rather than guessing
from the `.gob` extension.

Legacy gob images remain readable only when `AllowLegacyGob` is explicitly
enabled. The compatibility loader caps the input, requires exactly one gob
value with no trailing bytes, validates scope/counts/documents/terms/postings,
and produces the old mutable in-memory representation. Saving that recovered
index republishes it as `HOTLEX2`. Query serving and scoped local/hosted loaders
enable legacy recovery so existing volumes roll forward without an outage;
unscoped new code should use `LoadHotLexSnapshot` and leave legacy disabled.

Mapped images are immutable. A local write closes a mapped image and rebuilds
from the authoritative local chunk store; it does not expand the serving image
onto heap. Shard merges reject mixed base brain ids or generations before
publication. The projection CLI accepts `--generation-id` and its merge path
uses this strict validation.

```bash
# Build a generation-pinned image; .hlex is recommended for new automation,
# and explicitly create the gob-only rollback asset before cutover.
product-brain project-hotlex --neon --strip-text \
  --brain-id full-bench-v2 --generation-id <generation> \
  --format hotlex2 --out hotlex.hlex \
  --rollback-gob hotlex.rollback.gob

# Existing deployment paths continue to work.
OUROBOROS_ERB_HOTLEX_PATH=/hotlex/hotlex-full.gob \
OUROBOROS_ERB_HOTLEX_GENERATION=<generation> product-brain serve
```

### Rollback to a gob-only binary

1. Stop writers and the HOTLEX2-serving process. Confirm its load diagnostic
   said `format=hotlex2` and identify either the configured `--rollback-gob`
   output or the automatic `<serving-path>.rollback.gob` sidecar.
2. Validate the rollback asset with the current loader and the expected brain
   and generation; its `SnapshotFormat` must be `legacy-gob`. Keep a copy of the
   HOTLEX2 file for diagnosis.
3. Atomically rename/copy the rollback gob onto the exact path configured in
   the old binary, then restart that gob-only binary. Do not point the old
binary
   at HOTLEX2 bytes merely because the filename ends in `.gob`.
4. To roll forward again, stop the old writer, retain the rollback gob, publish
   a freshly scoped HOTLEX2 candidate, and verify the startup format/scope log.

The rollback image is a serving projection, not the authority. Local mode can
also rebuild from its durable chunk store. Tests exercise automatic
preservation,
explicit dual-write, decoding with the legacy gob wire shape, format
diagnostics,
and legacy fallback.

## ACL, citations, and blind evaluation

The format contains only corpus-derived lexical serving fields already present
in HotLex: chunk id, document id (`DSID`), source URI, optional passage text,
document length, terms, and postings. It has no principal, grant, evaluation
case, question type, reference answer, or gold-document field. Authorization
still occurs before retrieval in `AnswerOpts`; the mmap loader does not add an
alternate retrieval entry point. `DSID`, `SourceURI`, stored text, score, and
the `hot_lex` channel round-trip unchanged so downstream hydration, ACL,
grounding, and citation behavior remain on the existing product path.

Tests cover mapped-vs-mutable ranking equality, byte determinism, scope and
generation rejection, corruption/truncation, atomic last-good publication,
strict shard merges, legacy recovery/republish, citation identity, no-gold
surface, local ACL denial, and local reopen/recovery.

## Fixed local evidence

The committed receipt
[`hotlex-mmap-fixed.json`](../../stages/stage-09/evidence/hotlex-mmap-fixed.json)
records a fixed 20,000-document, 3,200,398-byte index-only fixture on Darwin
arm64. Scoped loader admission was 1 ms with 1,112 bytes allocated on the Go
heap; the fixture's exact-term lookup was 4 µs. An isolated test child recorded
a 3,276,800-byte peak-RSS delta and rejected a one-byte checksum corruption in
1 ms. A separate 10,000-document warm lookup
microbenchmark measured 269.8 ns/op, 200 B/op, and 7 allocs/op.

CI enforces broad architecture-independent regression ceilings rather than
those exact observations: load ≤2 s, Go heap allocation ≤512 KiB, isolated peak
RSS delta ≤64 MiB, exact-term lookup ≤50 ms, corrupt admission failure ≤2 s,
mapped state present, and heap corpus maps absent.

The receipt's scope is intentionally narrow. “Startup” means only
`LoadHotLexSnapshot` validation/admission; it excludes process/container boot,
mounts, networks, databases, and models. Peak RSS is an isolated child-process
high-water delta, not proportional set size or a production limit. Cache state
is uncontrolled, so the corrupt first-admission measurement is fail-closed
evidence, not cold-storage latency. No full500 run was performed: the synthetic
fixture has no full corpus, external stores, model, judge, or gold labels.
Finally, mmap eliminates corpus object-graph decoding, but digest and structural
validation scan every mapped byte before admission; this evidence makes no OS
demand-page/lazy-residency claim. Reproduce with:

```bash
go test ./services/brain/internal/hosted \
  -run '^TestHotLexMMapFixedMemoryLatencyEvidence$' -count=1 -v
go test ./services/brain/internal/hosted -run '^$' \
  -bench '^BenchmarkHotLexMMapLookup$' -benchtime=1000x -count=1
```
