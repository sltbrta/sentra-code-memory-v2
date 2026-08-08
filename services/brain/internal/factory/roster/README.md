# Stage 05 durable roster facts

Status: **[shipped] composed by the factory kernel.** This package owns the
durable Stage 05 leaf lease and leaf-result ledger facts on the migration 005
`factory_leases` and `factory_leaf_results` tables. It performs no I/O of its
own: every method runs inside a caller-owned `sql.DB` or `sql.Tx` (the
`Executor` port) so the composing factory kernel commits roster facts
atomically with run, plan, and idempotency facts.

## API

```go
store, err := roster.New(clock)
lease, err := store.Issue(ctx, ex, roster.Lease{...})             // next dense fence
current, found, err := store.Current(ctx, ex, t, p, run, node)    // sole possible winner
lease, err := store.Authorize(ctx, ex, t, p, run, node, fence)    // current + unexpired
result, err := store.CommitResult(ctx, ex, roster.Result{...})    // exactly-once canonical result
result, found, err := store.Result(ctx, ex, t, p, run, node)      // read canonical result
```

- `Issue` appends the next densely fenced lease for one leaf node. The
  schema's dense-fence trigger independently enforces `fence == max+1`, so
  two racing issuers cannot both win one fence; the highest fence is the
  only possible active winner.
- `Authorize` returns the current lease only when the presented fence is
  exactly the current fence and the lease is unexpired at the injected
  clock; anything else is `ErrStaleFence`. Commits under expired or
  superseded fences never become canonical.
- `CommitResult` authorizes the fence and records the leaf's canonical
  result atomically in the caller's transaction. An exact replay returns
  `Replayed` with the canonical digest as the authoritative match
  (vault-assigned artifact identities may differ across retries); a
  differing second result is `ErrResultConflict`; a stale fence is
  `ErrStaleFence`.

Run `bazel test //services/brain/internal/factory/roster:roster_test`.
