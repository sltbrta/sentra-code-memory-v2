# Ops profiles: local vs hosted

Two operating profiles exist for the brain stack. The standalone
`sentra-code-memory` binary is **local-only**; hosted profile features are
non-goals here (see `docs/roadmap/DEFERRED-AND-NON-GOALS.md`).

## Local profile (this product)

- Single-user, loopback-only. The HTTP adapter defaults to `127.0.0.1` and the
  MCP adapter is stdio; neither provides authentication.
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
