# Agent-facing workflow artifacts

`internal/workflow` produces machine-readable, bounded agent-facing artifacts
(Phase 5: issues #32–#34). Output is **deterministic** (stable field order,
lexical-count maps) and **content-safe** (digests and pointers, never source
bytes). The package depends only on the Go standard library, so it adds no
dependencies and does not touch the existing `codeserve` JSONL path.

## Action envelopes (#32)

`BuildEnvelope` wraps a verb payload with the bounded metadata an agent needs to
plan its next step: available actions, remaining budget, freshness, expansion
handles, confidence, coverage warnings, and verification commands. Actions and
handles are de-duplicated and deterministically ordered, so two builds from the
same inputs produce byte-identical JSON.

```go
env := workflow.BuildEnvelope("code_find_relevant", ok, payload,
    actions, handles, warnings, workflow.DefaultEnvelopeOptions())
```

## Evidence reports (#33)

`Build` assembles a reproducible report — context served, impact, tests, edits,
graph changes, unknown coverage, savings, verification, next actions — and
computes a content `Digest` so the same inputs always yield byte-identical JSON.
Edits and graph changes carry paths and digests only; no source bytes.

```go
r := workflow.Build(report, workflow.BuildOptions{Task: "phase4-5", Base: "abc123"})
fmt.Println(r.String()) // stable CLI line
```

## Candidate ChangeSet validation (#34)

`ChangeSet.Validate` is a **fail-closed** receipt. It freezes a base (tree ref +
per-file base digests), carries isolated candidate edits, and validates them
atomically. Any one of these gates rejects the whole set, so partial work never
lands:

| Gate              | Reason                  | When                                                  |
|-------------------|-------------------------|-------------------------------------------------------|
| empty             | `empty_changeset`       | no edits                                              |
| path escape       | `path_escape`           | absolute, backslash, or `..` path                     |
| bad range         | `bad_range`             | malformed `Start`/`End`                               |
| overlapping edits | `overlapping_edits`     | two edits share a range in the same file              |
| stale base        | `stale_base`            | frozen digest ≠ the edit's planned base digest        |
| partial failure   | `partial_failure`       | an edit's predicted digest ≠ observed digest          |

Each edit carries predicted/observed digests and the result includes a per-file
predicted-vs-observed `GraphDelta`, forwarded verification commands, and a
reproducibility `Digest`. Validation is pure (never reads/writes files).

```go
res := cs.Validate()
if !res.Accepted {
    // res.Rejected lists every RejectReason; res.Reasons gives detail.
}
```

## Transactional ChangeSet application (#45)

`ApplyChangeSet` turns the pure contract into a bounded local transaction.
`CandidateEdit.Range` is a zero-based byte range and `Replacement` is its exact
replacement. The caller supplies frozen per-file digests; when `root` is a Git
worktree root, `ChangeSet.Base` must also equal `HEAD`.

The implementation rejects non-canonical/escaping/symlinked paths, overlaps,
stale bases, oversized candidates, digest divergence, and failed verification.
It stages the complete tree in a detached Git worktree (or a bounded copy for a
non-Git directory), applies all edits there, runs at most eight bounded shell
verification commands, and only then promotes files using same-directory
atomic renames. A promotion-boundary failure restores every original file.
Receipts contain paths, digests, and command/output digests only—never source,
replacement text, or verifier output. `ApplyOptions.InjectFailureAt` is an
in-process test seam and is not exposed by any agent-facing protocol.
