# Factory effect broker

Package `effects` is the Stage 05 current-policy effect broker. The runner or
UI request is never authority: every brokered effect reauthorizes the current
identity, grant, policy, lease, fence, revocation epoch, and idempotency
immediately before execution.

## Grant shape

`Grant` and `Lease` mirror the frozen `CapabilityGrant` and `Lease` contract
messages: exact action and resource lists (no wildcards), normalized allowed
path prefixes, the exact pinned `repository_git_oid`, nonce, revocation
epoch, expiry, policy digest, command fence, and the fenced lease. A leaf
grant carrying `factory.dispatch` or `factory.task.create` is malformed and
denies everything, matching the frozen no-redispatch contract rule.

## Checks, in order

Identity match, grant validity and expiry (always against the broker's live
clock — a caller-supplied `Request.Now` is recorded but never evaluated, so
no caller can authorize against an earlier instant after expiry), base
binding between scope and grant, dispatch/task-create denial, sealed-surface
action vocabulary (only `file.read`/`file.write` are executable in v1),
owned/forbidden/grant path scope, lease fence currency from the
`FenceRegistry` port, current policy via the `contracts.PolicyCheck` port
with epoch equality, and — for `Execute` — idempotent exactly-once execution
namespaced by tenant and grant identity (two grants may reuse one key). An
exact replay still reauthorizes against current policy first (a revoked retry
denies; it never replays). An in-flight placeholder under the broker lock
makes exactly-once hold for concurrent same-key calls: the second caller is
rejected rather than serialized, and a failed mutation stays claimed so a
retry cannot double-execute.

`Broker.Bind(leaf)` returns the `MutationAuthorizer` the candidate store
calls before every candidate mutation, so lease, fence, and grant are
rechecked at mutation time rather than only at admission.

Denials are static typed `Denial` values with internal reason codes;
`escape_*` codes mark escape attempts that fail the run closed. Public edges
collapse every denial to `not_found_or_denied`.

Acceptance label: `//services/broker/internal/factory/effects:effects_test`.
