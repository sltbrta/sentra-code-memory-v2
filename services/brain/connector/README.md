# connector

Active connector-owned composition boundary.

`Surface` and `QueryAuthenticated` provide the RPC-neutral lane for trusted
gateway, worker, or CLI adapters. The query command is exact and bounded and
requires an independently authenticated principal; delegated grant issuance
uses a separate `DelegatedIssuerPort`. Opaque queries additionally require a
live provider, durable grant store, audit sink as appropriate, complete source
freshness, and an independent `DelegatedPromotionGate`.

This package does not expose a product RPC for delegated grants. The current
protobuf query request has no grant field, so the default gateway does not
enable opaque scopes. It also contains no default promotion allow and makes no
claim that a live provider is production-certified. New connector composition
belongs here, not in the retired `services/brain/localauthority` boundary.
