# Local authority gateway

This package exposes the three frozen Stage 02 protobuf procedures and, when
the corresponding authority ports are supplied, the five frozen Stage 03
ingestion procedures and the four frozen Stage 04 query procedures over an
owner-only Unix socket. It authenticates the operating-system peer before
`net/http` can read request bytes, cross-checks body identity against that
peer, validates the complete generated request and response messages,
enforces request and active-connection bounds, and returns only static public
errors.

## Public surface

- `NewServer` validates the socket and injected authority configuration.
- `PeerMapper` is the composition surface for trusted peer-to-principal mapping.
- `Server.Serve` owns Unix listener creation, graceful shutdown, and safe socket
  cleanup.
- `Authority` is the narrow composition port for canonical session, command,
  and status behavior.
- `IngestionAuthority` is the optional composition port for add, status, code
  search, reconcile, and revoke behavior. Its routes remain static 404s until
  the Stage 03 command adapter supplies an implementation.
- `QueryAuthority` is the optional composition port for ask, sources, history,
  and status behavior. Its routes remain static 404s until the Stage 04
  command adapter supplies an implementation.

The package deliberately has no TCP fallback, database, policy implementation,
artifact bytes, TUI composition, or daemon entry point. Idempotency and current
authorization decisions remain canonical authority responsibilities; this
gateway validates their required request fields and passes them through.
Stage 03 ingestion requests and responses run through the generated protobuf
descriptors and the repository-pinned Protovalidate runtime. The caller's
principal, tenant, and session are cross-checked against the authenticated peer
before authority invocation; authorization and source-existence decisions stay
behind `IngestionAuthority`.

Every existing socket ancestor is checked for symlinks, unsafe write modes, and
unexpected ownership at construction and immediately before listen. A writable
ancestor is accepted only when it is a real root-owned sticky directory such as
`/tmp`; the immediate socket directory remains current-user-owned mode `0700`.
This boundary reduces accidental replacement; it does not claim to stop root or
same-user malware racing those checks.

Darwin and Linux are the v1 supported local transports. Darwin peers are
authenticated through `LOCAL_PEERCRED`; Linux peers through `SO_PEERCRED` with
identical accept-time semantics. Other operating systems fail closed until they
receive a reviewed peer-credential implementation.

Run `go test -race ./gateway/internal/localauthority` from `services`, or
`bazel test //services/gateway/internal/localauthority:localauthority_test`.
