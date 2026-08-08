# Stage 05 durable mailbox facts

Status: **[shipped] composed by the factory kernel.** This package owns the
durable Stage 05 inter-agent message facts on the migration 005
`factory_mailbox_messages` and `factory_mailbox_acks` tables. Like the roster
package it performs no I/O of its own; every method runs inside a
caller-owned `sql.DB` or `sql.Tx` so the composing factory kernel commits
messages atomically with run facts. Message payload bytes live in the
encrypted ArtifactVault; only identities and digests enter the ledger.

## API

```go
store, err := mailbox.New(clock)
sent, err := store.Send(ctx, ex, mailbox.Message{...})            // dense per-task sequence
pending, err := store.Pending(ctx, ex, t, p, run, task)           // unexpired, ack-joined
replayed, err := store.Acknowledge(ctx, ex, t, p, run, messageID) // exactly-once ack
```

- `Send` appends one message at the next dense per-(run, task) sequence; the
  schema trigger independently enforces density. Message identities are
  replay-safe: an exact resend — same identity, task, kind, and payload
  digest — collapses to the original sequence with `Replayed`, with the
  canonical digest as the authoritative match (vault-assigned artifact
  identities may differ across retries); a same-identity send with a
  different payload or kind is `ErrMessageConflict` and mutates nothing.
- `Pending` lists unexpired messages in dense sequence order joined with
  acknowledgement state; expired messages stay canonical but are never
  delivered as current guidance.
- `Acknowledge` commits one durable acknowledgement; repeat
  acknowledgements return `replayed`, and unknown messages are
  `ErrUnknownMessage`.

Run `bazel test //services/brain/internal/factory/mailbox:mailbox_test`.
