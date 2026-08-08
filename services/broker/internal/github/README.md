# GitHub draft-PR effect broker

Status: **[partial] Stage 06 L2 — two-phase draft PR only.** Fine-grained PAT
env surface (`GITHUB_TOKEN` / `OUROBOROS_GITHUB_TOKEN`). Unit tests use
`FakeAPI` and never contact the network. No merge, deploy, release, force-push,
or branch-delete actions are expressible.

## Protocol

```text
branch_pending → branch_in_flight → branch_published
  → pr_pending → pr_in_flight → pr_created
```

1. **Phase 1 — branch:** reauthorize → check base still at approved OID →
   create deterministic head ref
   `refs/heads/ouroboros/tracer-001/<24-hex>` only when absent; equal OID is
   success; different OID is terminal conflict (never force-updated).
2. **Phase 2 — draft PR:** reauthorize → recheck base →
   **lookup-before-create** (open and closed) → create draft only when zero
   matches; exact open draft reconciles; closed exact PR is
   `PR_ALREADY_CLOSED` (no duplicate); non-draft / content mismatch / multiple
   matches are terminal conflicts.

Crash after either phase re-enters via the same publication-tuple key and
converges to one head ref + one draft PR + one receipt.

## API

```go
broker, err := github.NewBroker(github.Config{API: fake, Policy: policy})
receipt, err := broker.Publish(ctx, github.PublishRequest{...})
token := github.ResolveToken() // GITHUB_TOKEN | OUROBOROS_GITHUB_TOKEN
```

## Tests

```text
bazel test //services/broker/internal/github:github_test
```
