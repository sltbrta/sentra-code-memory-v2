# Local authority broker facade

This package is the public Stage 2 composition boundary over the local peer
identity mapper, OpenFGA-shaped relationship evaluator, and attenuated
capability engine. Every effect must call `Broker.Authorize`; capability success
does not cache or replace current policy.

The facade accepts one fixed UID, principal, tenant, and session. It maps
operating-system peer facts before body decode and returns one non-disclosing
denial for wrong peers, malformed grants, absent relationships, and stale
revocation epochs.

Request-body grants are claims, not authority. Trusted composition explicitly
registers each exact bounded issued grant in memory; authorization resolves the
grant ID and requires every structural authority fact to match before use.
Restarts intentionally begin with an empty registry and must reinstall trusted
configuration. There is no wildcard, default, or caller-created grant path.

Default composition binds the in-process `authz.RelationshipStore` adapter and
never auto-selects a remote OpenFGA endpoint. Trusted composition may inject an
`authz.Client` via `NewWithStore` when a live URL is intentionally configured;
missing configuration stays fail closed on the in-process path.

This is not a hosted OpenFGA implementation, a grant issuer, or a durable policy
store. Trusted startup/configuration code owns relationship and issued-grant
loading. Stage 2 uses the checked local policy subset only. Live store lifecycle
and policy administration remain DEF-015 / DEF-013 residuals.
