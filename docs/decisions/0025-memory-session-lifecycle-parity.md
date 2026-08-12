# 0025 — Memory/session/lifecycle parity: expose, defer, or retire

## Status

Accepted (2026-08-13). Closes the P2 parity decision in issue #47.

## Context

Issue #47 observed that the Go packages already contain `sessionlog`,
`savings`, memory tiers, and workflow evidence, but the canonical codeserve
surface exposed only `memory_ask`. Prior SCM shipped typed memory tools,
lifecycle/server/hooks commands, and richer query modes. The standalone product
must decide, surface by surface, what it owns and expose it with contracts — or
disclose it as deferred/non-goal and stop advertising it.

The post-P6 audit (`docs/research/2026-08-12-parity-audit-and-remaining-work.md`)
flagged these as "Reachability/doc conflict" and "Missing from standalone
operator surface". ADR 0023 already keeps the full SCM session product and
hosted/cloud tenancy out of this program.

## Decision

Adopt the parity decision table below. Reachable surfaces are exposed through
`codeserve` (JSONL), the CLI, HTTP, and MCP with conformance tests. Deferred and
non-goal surfaces are catalogued for discoverability but return a structured
deferred disclosure (`error_code: "deferred"`, `deferred: true`) instead of an
opaque unknown-verb error, and are not advertised as callable MCP tools.

### Parity decision table

| Surface | Prior SCM capability | Decision | Standalone exposure |
| --- | --- | --- | --- |
| Typed memory admit | memory tools add/entity/fact | **Expose** | `memory_put` (principal-gated, tier stm/mtm/ltm) |
| Typed memory recall | typed recall / search | **Expose** | `memory_search`, `memory_list` |
| Memory lifecycle | promotion/archival tiers | **Expose** | `memory_promote` |
| Session continuation | continuation packets | **Expose bounded composite** | `session_continuation` (budgeted packet over local `sessionlog`) |
| Token savings | savings ledger | **Expose** | `savings_summary` |
| Full session product | latent dev-state memory, agent runtime | **Defer** | `session_product` → deferred disclosure |
| Lifecycle/install | server/index/login/hook/uninstall | **Defer** | `lifecycle_install` → deferred disclosure |
| Dense/reranked retrieval | dense vectors + rerank | **Defer** | `code_dense_rerank` → deferred disclosure |
| Prior query modes | patch plans, test hints, greenfield | **Defer** | `query_advanced` → deferred disclosure |
| Hosted/cloud/tenancy/billing | hosted roadmap | **Non-goal** | `hosted_tenancy` → non-goal disclosure |

The bounded local operators reuse already-implemented, tested packages
(`memory.Store` agent-memory tiers, `sessionlog.BuildContinuation`,
`savings.Ledger`). No new retrieval heuristics or network paths are introduced.

### Deferred disclosure contract

Every deferred/non-goal verb is listed in `codeserve.Catalog()` and in
`CatalogMetadata()` with `status: "deferred"`, but has no handler. Calling one
returns `ok:false` with `error_code:"deferred"`, a `decision` (`deferred` or
`non_goal`), a `reason`, and a `doc` pointer. MCP `tools/list` omits
non-stable verbs, so agents never see them as callable; JSONL/CLI/HTTP callers
still get the structured disclosure. This keeps the gap discoverable and honest
rather than silently missing.

## Consequences

- Reachable capabilities are covered by CLI/JSONL/MCP tests
  (`handle_memory_session_test.go`, `main_memory_test.go`,
  `adapters_memory_test.go`).
- Deferred capabilities carry surfaced 501-class disclosures and matching docs
  (`docs/roadmap/DEFERRED-AND-NON-GOALS.md`,
  `docs/specs/product/SCM-SESSION-PRODUCT.md`).
- Broken README links are repaired so no capability is falsely advertised.
- Hosted/cloud/session-product remain explicit non-goals per ADR 0023.
