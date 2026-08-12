# SCM session product (deferred, non-goal for standalone)

Status: **deferred / non-goal** for the standalone `sentra-code-memory` binary.
This spec exists so code comments and READMEs that mention the "SCM session
product" link to one honest disclosure (issue #47, ADR 0023/0025).

## What the SCM session product is

The prior SCM repositories shipped a session product class distinct from code
operators:

- **Agent continuation packets** as a first-class product: resumable
  development state carried across agent sessions.
- **Latent development-state memory:** long-lived memory of the agent's own
  task trajectory, separate from repo content.
- Session APIs and MCP `memory-*` tools for typed recall of entities, facts,
  preferences, relationships, messages, and reasoning traces at product scale.

That is a different product class from a local code-memory operator, and ADR
0023 keeps it out of this program.

## What the standalone product ships instead

The standalone binary exposes a **bounded local composite**, not the session
product:

- `session_continuation` (codeserve verb / CLI `session-continuation`): reads
  the repo-local `sessionlog` event stream and folds it into a budgeted
  continuation packet via `internal/sessionlog.BuildContinuation`. It is
  offline, deterministic (with an injected `now`), privacy-safe (pointers, not
  content), and returns structured freshness/budget metadata.

The local building blocks — `sessionlog` (bounded append-only event log,
provenance-first admission, recall with abstention, continuation packets) — are
implemented and tested; they are surfaced only through that bounded composite.

## Explicitly out of scope

- Hosted session sync, tenant-scoped sessions, and cloud overlays.
- The full 23/24-tool memory-compatible MCP contract.
- Latent development-state memory beyond the bounded local event log.

Invoking the deferred `session_product` codeserve verb returns a structured
disclosure (`error_code: "deferred"`) rather than implementing any of the above.
See `docs/roadmap/DEFERRED-AND-NON-GOALS.md`.
