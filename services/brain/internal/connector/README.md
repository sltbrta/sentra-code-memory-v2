# connector

Stage 08 source-connector **kernel** (GitHub connect semantics). Served via
gateway connectorapi + authorityprocess. Product CLI surface remains authority RPC.

## Delegated-permission retrieval for ACL-opaque scopes (issue #309)

`delegated.go` adds an opt-in `DelegatedGate` (`Config.Delegated`) for source
scopes whose provider ACLs cannot be projected at ingest time. For a gated
scope, `QueryConnectorEvidence` never serves admitted evidence on projection
membership alone:

- **Authenticated explicit grants** — the stable `DelegatedGrantPort` issues and revokes an
  exact tenant/principal/scope `DelegatedGrant` (no wildcards, bounded
  lifetime, single-use ID). `DelegatedIssuerPort` resolves the administrator
  exclusively from trusted transport/session context; grant payload fields are
  never issuer authentication. `DelegatedGrantStore` is persistence/RPC-neutral;
  its bounded memory implementation is only the reference adapter. Stores
  prepare grants inactive, audit them, and only then activate them, so failed
  audit cannot leave usable authority.
- **Bounded network/freshness** — at most `MaxProbesPerQuery` live
  candidates enter the provider phase. Each permission or content call is
  under `ProbeTimeout`; permission verdicts are fresh at most `VerdictTTL` and
  never past grant expiry. Over-budget or unverifiable objects are denied.
- **ACL before fan-out; authority after hydration** — the exact grant is read
  and validated before the opaque connection's projection is enumerated. For
  each candidate, `DelegatedProvider` performs a permission probe, exact-ID
  content hydration, and an uncached permission recheck. Content must match
  the admitted immutable object/revision. The grant store and connection ACL
  epoch are re-read before content can reach answer construction.
- **Scoped cache** — verdicts key on tenant|principal|scope|grant|epoch|object;
  revocation bumps the epoch and purges that grant's verdicts.
- **Audit receipts** — issuance, revocation, preauthorization denials,
  permission decisions, and hydration decisions append sanitized hash-linked
  `DelegatedReceipt` values. Tenant, principal, source, and grant identities
  are domain-separated digests; query text, object IDs/content, and tokens are
  absent. `DelegatedAuditSink` is persistence/RPC-neutral and bounded by
  `AuditTimeout`; an append failure makes the associated operation fail closed.
- **Source freshness** — every opaque query pins the connector component digest,
  exact revision/cursor, and provider observed-at. Receipts retain the connector
  digest, domain-separated revision/cursor digests, and observed-at timestamp.
  Missing or future freshness blocks before object enumeration.
- **Independent component gate** — every opaque query passes versioned
  `DelegatedComponentEvidence` to an injected `DelegatedPromotionGate`. Missing
  or rejected evidence fails closed. This is the typed bridge to issue #307;
  the package ships no default allow and makes no live-provider production
  certification claim.
- **Probes never hold kernel locks** — `QueryConnectorEvidence` captures the
  connection facts (id, scope, ACL epoch) under the kernel mutex, releases it
  for the live probes, then reacquires and revalidates the lookup and ACL
  epoch before building any answer. A revoke/purge that lands mid-probe wins:
  the query returns the non-disclosing denial, and a slow or hung provider
  can never block `RevokeConnector` or any other lifecycle operation
  (regression: `TestRevokeConnectorNotBlockedBySlowProbe`).
- **Issue-time window checks** — a grant with a future `IssuedAt` is rejected
  as invalid. Stores retain revoked IDs so they cannot be resurrected; the
  memory reference store bounds this lifetime set.

With `Config.Delegated == nil` (the default gateway composition) behavior is unchanged.

## Active connector boundary and RPC contract limit

`services/brain/connector` is the active, connector-owned public composition
boundary. The retired `services/brain/localauthority` package does not export
delegated types, constructors, or configurable connector composition.

`DelegatedProvider`, `DelegatedGrantStore`, `DelegatedIssuerPort`,
`DelegatedPromotionGate`, and `DelegatedAuditSink` are the only live adapter
ports. A provider adapter must use the caller-bound provider
credential to check exact-object read permission, fetch that exact object, and
check permission again. It must honor context deadlines and must not fall back
to provider search, public web content, a service/admin credential, or stale
projected bytes. The connector package performs no network access itself.

The current protobuf `QueryConnectorEvidenceRequest` has no delegated grant
field and no grant-administration RPC. The default authority-process gateway
therefore cannot expose delegated retrieval and does not enable an opaque
scope. The RPC-neutral Go lane uses `AuthenticatedPrincipalPort` plus the exact
bounded `AuthenticatedQueryCommand` grant ID through
`services/brain/connector.QueryAuthenticated`; a future authenticated RPC
adapter may map to it only after the protobuf contract gains an explicit
field/RPC. This is a public schema limit, not an authorization fallback: opaque
queries without an exact grant and all required ports abstain.
