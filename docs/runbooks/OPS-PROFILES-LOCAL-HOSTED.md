# Ops profiles: local vs hosted

Two operating profiles exist for the brain stack. The standalone
`sentra-code-memory` binary is **local-only**; hosted profile features are
non-goals here (see `docs/roadmap/DEFERRED-AND-NON-GOALS.md`).

## Local profile (this product)

- Single-user, loopback-only. The HTTP adapter defaults to `127.0.0.1` and the
  MCP adapter is stdio. The HTTP adapter takes a bearer token via `--token` or
  `SENTRA_CODE_MEMORY_HTTP_TOKEN`, and refuses a non-loopback bind without one
  unless `--allow-insecure` is passed. Both surfaces confine requests to a
  subtree: `--root` defaults to the working directory, and `--root=/` is the
  explicit opt-out. Mutating verbs additionally require an out-of-band operator
  grant (`--operator-trust`, `SENTRA_CODE_MEMORY_OPERATOR_TRUST=1`, or the
  `X-Sentra-Operator-Trust: 1` header); no request field, query parameter or
  tool argument can supply it. A second form, `?operator_trust=1`, was
  accepted until 2026-08-21 and contradicted that sentence: a query parameter
  is part of the request line the caller composes, and it put the grant into
  every access log the URL reached.
- All state is on the local filesystem: the durable `code-index.gob`, the
  repo-local `sessionlog`, the memory projection, and the savings ledger.
- Credentials are optional and only for the local/hosted model clients used by
  the memory path; code operators need none.
- Trust boundary: the caller is trusted. `code_read` is root-bounded but reads
  obey the local caller, not an ignore/ACL policy.

## Hosted profile (not shipped here)

The hosted profile — managed multi-tenant serving, remote overlays, tenant-scoped
source bundles, authn/authz, cloud sync, and billing — belongs to the broader
product-brain program, not this standalone binary. It is an explicit non-goal.
The hosted model/embedding/rerank clients retained in the extracted backend are
optional local integrations; they do not imply a hosted control plane.

## Choosing

- Coding agent needing fast local code memory and retrieval: **local profile**.
- Organization-wide multi-tenant RAG with governance: out of scope for this
  repository; see the deferred/non-goals disclosure.
