# authorityprocess

Product-owned **local authority process** library (ADR 0022).

## Entry

```bash
product-brain authority --bootstrap /abs/manifest.json --bootstrap-sha256 <hex64>
```

Serves peer-authenticated Unix-socket RPCs: LocalAuthority, Ingestion, Query,
Factory, Meeting, Multimodal, Connector, Tracer001.

## Not a second product brand

Company residual ask lives on `product-brain ask`. This process is the ACL /
Git / vault / factory half of the product superset.

## Tests

Unit: config/adapter/query (no Bun). Process tracers need `OUROBOROS_BUN_BIN`.

Historical cmd snapshot:
`archive/2026-07-stage-retirement/…/ouroboros-local-authority`.
