# Factory Git candidate

Package `gitcandidate` is the Stage 05 exact-base atomic Git candidate store.

## Authority

A candidate is an isolated repository beneath a configured candidate root,
hydrated from the canonical approved root at one exact pinned base commit.
Canonical objects are read through a read-only `.git/objects/info/alternates`
entry; the candidate has its own object database, index, refs, config, and
worktree, and every write stays candidate-local. The canonical worktree plus
the complete `.git` inventory (bytes, modes, sizes) must stay byte-identical
across success, failure, and discard; `AttestCanonical` computes that
inventory attestation and the tests compare it before and after every case.

## Behavior

- `Store.Begin` verifies the base resolves to a commit, hydrates a detached
  candidate, and proves the worktree equals the base tree exactly.
- `Store.Apply` validates the edit set (`changeset.Validate`), reauthorizes
  every mutation through the `MutationAuthorizer` port at mutation time,
  verifies pre-image bytes against pinned before digests, applies edits in
  declared order, re-verifies post-image bytes against after digests, and
  computes the post-image digest plus a candidate-local Git tree OID.
- Application is all-or-nothing: any failure — including a set-level
  validation rejection before the first mutation — removes the complete
  candidate directory, and the rejected outcome carries a rollback receipt
  (rejected status, static reason code, candidate/base/changeset binding,
  discarded edit count, failed edit index).
- Paths resolve component by component; any symlink component denies, so
  edits cannot escape the candidate root. Applications serialize per store,
  so every candidate tree is single-writer by construction. Residual risk:
  an external process with write access to the candidate root could swap a
  directory for a symlink between the component check and the mutation; the
  candidate root is owner-only (`0700`) beneath a controlled parent and
  post-image verification re-resolves every touched path, so such tampering
  fails the run closed on detection — but it is out of the trust model, not
  fully excluded.
- Idempotency is scoped by tenant, principal, and key, matching the
  canonical command-record scope: one principal's exact replay returns the
  recorded outcome without re-executing, a conflicting reuse within one
  scope denies, and a different principal under the same key is an
  independent scope that computes its own candidate. Concurrent same-scope
  applications deny; no removal ever touches an in-flight candidate.
  Idempotency state is a process-local projection; durable replay state
  belongs to the kernel ledger. Post-image digests cover path, mode, and
  content bytes.

Git runs scrubbed (`--no-optional-locks`, hooks, credentials, fsmonitor,
system config, maintenance, and prompts disabled; `HOME=/nonexistent`), with
bounded output and a command timeout, matching the Stage 03 ingestion
discipline.

Acceptance label:
`//services/broker/internal/factory/gitcandidate:gitcandidate_test`.
