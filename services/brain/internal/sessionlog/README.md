# Local session event log

`internal/sessionlog` records a repo-local, append-only, bounded, privacy-safe
event stream for coding-agent sessions (Phase 4: issues #26–#31).

The log is **opt-in**: the fast lexical `codeserve`/`codecrawl` path never
touches it, so existing JSONL behavior is unchanged. Callers that want session
continuity append one event per observable step and later replay the stream.

## Events

Supported kinds (closed set; unknown kinds fail closed on append and replay):

`task_start`, `context_served`, `read`, `refresh`, `edit`, `test`, `failure`,
`compaction`, `completion`.

```go
w, err := sessionlog.Open(repoLocalDir, sessionlog.WithMaxEvents(2048))
sealed, err := w.Append(sessionlog.Event{
    Kind: sessionlog.KindEdit, Session: "task-7",
    Verb: "code_edit", Freshness: sessionlog.FreshnessAsOf,
    Provenance: sessionlog.Provenance{
        Repository: "local", Tree: "abc123",
        Path: "a/b.go", Range: sessionlog.Range{Start: 12, End: 14},
        Symbol: "Alpha", Confidence: 0.9,
    },
    PredictedDigest: "pred…", ObservedDigest: "obs…",
})
```

## Properties

- **Bounded**: a single `<dir>/session-events.jsonl` capped at `MaxEvents`
  (default 2048). Overflow is folded into one recorded `compaction` event via a
  temp-file + sync + atomic rename, so readers never see a torn log.
- **Privacy-safe**: events prefer pointers (`repository/tree/path/range/symbol/
  handle`) over copied source. `FreeText` is the only free-form field and is
  capped (`MaxFreeTextBytes = 512`). Absolute, backslash, and `..` path escapes
  are rejected.
- **Replayable**: `Replay(events, applier)` and `Rebuild(events)` are the
  deterministic event-to-projection paths (#31). A summary built live (one
  `Apply` per appended event) always equals `Rebuild(Writer.Events())`.
- **Provenance-first admission (#29)**: durable kinds (`edit`, `compaction`,
  `context_served`) require a repository/tree identifier and a path/symbol/handle
  locator; an event missing provenance is rejected on append.
- **Freshness & supersession (#28)**: every freshness class
  (`timeless`/`as_of`/`pointer`/`stale`/`superseded`) is validated; superseded
  content is excluded by default.
- **Recall with abstention (#30)**: `Recall(events, query, opts)` admits memory
  only above confidence and relevance thresholds and abstains otherwise, so weak
  or unrelated memory is never injected.

## Continuation and compaction packets (#27)

`BuildContinuation` folds events into a resumable summary with read ranges,
changed files, unresolved questions, failures, next actions, and freshness
warnings, with L0–L3 byte budgets (L0 immutable pointers, L1 fresh, L2
stale-but-valid, L3 compacted). Lower tiers are admitted before higher tiers;
resumed work reuses pointers instead of repeating context.

```go
cont, err := sessionlog.BuildContinuation(w.Events(), base,
    sessionlog.DefaultContinuationOptions(), time.Now())
```

No new dependencies; the package uses only the Go standard library.
