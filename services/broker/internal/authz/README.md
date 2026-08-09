# Local authorization evaluator and OpenFGA adapters

This package implements the exact Stage 02 relationship subset represented by
the checked-in OpenFGA model: evidence inherits reader/admittor/deleter from an
explicit brain relationship, brain owner may read/write, and brain viewer may
read. Tenant membership alone conveys no authority. Every check also requires
the caller's observed revocation epoch to equal current state. Stage 03 source
actions (`source.*`) and the Stage 04 query-funnel checkpoints (`query`,
`hydrate`, `emit`) evaluate the same current brain relationship: mutation is
owner-only, while reads and query checkpoints allow owner or viewer.

## RelationshipStore

`RelationshipStore` is the Broker-facing surface (`Check`, `CheckSource`,
`Write`, `Delete`, `SetEpoch`, `Epoch`). Two implementations ship:

| Adapter | Role |
| --- | --- |
| `InProcessAdapter` | Default fail-closed path. Wraps the fixture-compatible `Evaluator`. Used by `localauthority.New`. |
| `Client` | OpenFGA HTTP adapter. Speaks `/stores/{id}/write` and `/stores/{id}/check`. Application deny epochs and tenant-scope mirrors stay local. |

Default composition never auto-selects a remote store. `NewClientFromEnv` only
returns a client when `OUROBOROS_OPENFGA_API_URL` is set (also requires
`OUROBOROS_OPENFGA_STORE_ID`; optional `OUROBOROS_OPENFGA_MODEL_ID` and
`OUROBOROS_OPENFGA_API_TOKEN`).

## Conformance

- Hermetic dual-run: personal and company fixtures run against the in-process
  adapter and against `Client` pointed at a test-only fake OpenFGA HTTP server
  (`dual_run_test.go`).
- Elevated live path: when `OUROBOROS_OPENFGA_API_URL` is set, the same company
  fixtures run against the configured server. Live durable-store lifecycle and
  full external-service conformance remain
[DEF-015](../../../../docs/roadmap/DEFERRED.md)
  residual (issue #72 partial).

Policy administration UI is out of scope
([DEF-013](../../../../docs/roadmap/DEFERRED.md)).
