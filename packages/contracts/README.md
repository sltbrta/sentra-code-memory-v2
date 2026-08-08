# Ouroboros canonical contracts

Status: **[backend contract ready, runtime pending]**. This package provides
versioned message schemas, generated Stage 02–05 Connect boundaries, the Stage
06 Tracer 001 message freeze (no product RPC), the Stage 07 MeetingService
transcript-import boundary, the Stage 08 ConnectorService GitHub source
surface, and language bindings only; it does not expose a
listening runtime, authorization service, or user surface.

`proto/` is the internal source of truth. The package defines versioned
cross-domain messages that later Go services, the TypeScript factory, Swift
clients, workers, and public-edge adapters share. Generated Go, TypeScript,
and JSON Schema projections are checked in so consumers can review exact API
shape without treating a language binding as authority.

`generated-manifest.json` binds every generation input, normalization script,
and checked-in output by path, size, and SHA-256. Ordinary `just check` verifies
that manifest without calling remote plugins; `just generate` is the only root
command that refreshes generated files and the manifest.

## Authority and boundaries

These contracts describe facts, requested transitions, evidence, and scoped
authority references. They do not authorize a caller, mutate canonical state,
read an artifact, resolve a secret, or execute an external effect. Implementers
must obtain actor identity from their authenticated boundary, reauthorize a
current `CapabilityGrant` immediately before an effect, reject malformed or
stale input, and emit a non-sensitive `Receipt`.

`BrainQuery.requested_principal` and comparable body fields are intentionally
untrusted values to cross-check. `ArtifactRef`, `EvidenceRef`, `Lease`, and
`Approval` are references, not bearer access. A `ChangeIntent` requests a
candidate change; only a separately authorized draft-PR effect may create a
remote artifact.

## Contract map

| Proto file | Main contracts |
| --- | --- |
| `common.proto` | envelopes, causal IDs, sequence, fence, watermark, idempotency, receipts |
| `evidence.proto` | revisions, multimodal anchors, renditions, lineage, tombstones |
| `security.proto` | capabilities, tools, egress, authorization receipts, effect approval |
| `brain.proto` | grounded queries, claims, citations, coverage, abstention |
| `connectors.proto` | source/action connectors, snapshots, deltas, reconcile, ACL, deletion; Stage 08 ConnectorService (GitHub source connect/status/reconcile/query/revoke/purge) |
| `factory.proto` | schedules, Workflow IR, roster, mailboxes, progress events, handover, ChangeSets, draft PR action; Stage 05 bounded intent admission, one-layer plan, candidate preview, findings, cancel |
| `tracer.proto` | Stage 06 Tracer 001 records: manifest, variants, run/step receipts, review disposition, draft-PR receipt, outcome facts, `VERIFY_TRACER_EVIDENCE_SHARD_V1` input/output and logical parity projection (no product RPC; no merge/deploy) |
| `tools.proto` | ToolPackageV1, observed capabilities, certification, registry state |
| `meetings.proto` | meeting sessions, segments, utterances, screenshots, provider artifacts; Stage 07 bounded import/status/query/revoke/purge service |
| `configuration.proto` | snapshots, layered origins, secret references, application mode |
| `channels.proto` | channel gateways, identity mapping, replay, action reauthorization |
| `local_authority.proto` | Stage 02 local session, typed artifact authority commands, status, Connect service |
| `ingestion.proto` | Stage 03 approved-root add/status/search/reconcile/revoke, committed snapshots, P5 readiness and occurrences |
| `query.proto` | Stage 04 grounded ask/sources/history/status, exact citations, freshness, coverage, abstention, private turns |

## Generated projections

`buf.gen.yaml` pins only verified remote generation plugins and writes:

- `gen/go/` — Go Protobuf message bindings;
- `gen/ts/` — Protobuf-ES TypeScript message bindings;
- `gen/jsonschema/` — strict bundled Draft 2020-12 projections for the selected
  public-edge envelopes, query/answer, change, channel, local-authority, ingestion,
  grounded-query, and factory request/response types. Other internal messages remain
  Proto-only until a public edge actually needs them.

`local_authority.proto` preserves the three Stage 02 operations.
`ingestion.proto` adds only the five Stage 03 one-shot TUI operations. It accepts
no filesystem path or client-authored root ID, implicitly selects the sole
preapproved bootstrap root, pins committed Git inputs, and leaves authentication,
policy, SQLite, indexing, and ArtifactVault execution to later runtime leaves.
`query.proto` adds only the four Stage 04 bounded query views. It pins one
immutable Stage 03 generation per question, requires exact code anchors and
model-proposal claims on every emitted claim, collapses denied and revoked
support into the same public `absent_support` reason, and leaves retrieval,
synthesis, session storage, gateway, and TUI execution to later runtime leaves.
`meetings.proto` adds the five Stage 07 bounded meeting operations
(`ImportTranscript`, `GetMeetingStatus`, `QueryMeeting`, `RevokeMeeting`,
`PurgeMeeting`). It requires explicit retention and a participant-notification
reminder acknowledgement on import, returns time-range citations on query, and
collapses unknown, unauthorized, revoked, and purged meetings into the static
`not_found_or_denied` shape. Live capture, ScreenCaptureKit, calendar
automation, and provider SDK capture remain deferred (`DEF-002`).
`factory.proto` adds only the five Stage 05 bounded factory operations. It caps
the typed DAG at one orchestrator with at most three disjoint-write-scope
non-recursive leaves, pins every leaf grant to the exact intent Git base without
dispatch authority, makes verified candidate states unreachable behind failed
required gates, requires a rollback receipt on every rejected candidate, covers
each touched P5 language with exactly one impact/docs/test obligation, collapses
stale, revoked, conflicting, and unauthorized runs into the same public
`not_found_or_denied` reason, and leaves planning, runners, effects, candidates,
review, gateway, and TUI execution to later runtime leaves.
`tracer.proto` freezes Stage 06 Tracer 001 typed records only. It pins the
manifest identity tuple, five co-equal variants, ten causal steps, different-
family review disposition, two-phase draft-PR receipt (draft-only), sanitized
outcome facts (machine observation; raw traces separated), and the sole Modal
operation `VERIFY_TRACER_EVIDENCE_SHARD_V1` with logical parity projection. It
introduces no product RPC and no merge/deploy effect messages; Stages 03–05
services remain the product surface.

`CapabilityGrant` schema validation requires bounded exact actions/resources,
identity, nonce, expiry, policy digest, and command fence and rejects wildcard
action shape. Expiry freshness and initiator-to-authenticated-peer equality are
runtime checks owned by the Stage 02 authority kernel; the schema and fixtures
name those denials without claiming static validation can evaluate current time
or authenticated transport state.

For binary compatibility, `CapabilityGrant.repository_git_oid` retains proto3
implicit presence. The strict JSON projection therefore requires the property;
an empty string means the optional repository context is absent. Task, workflow,
and lease contexts remain optional message fields.

## Generate and verify

Use the root facade with its pinned toolchain:

```sh
just check
just generate
```

`just check` validates the tracked source/output digest manifest, Buf STANDARD
lint, descriptor build, FILE breaking compatibility against
`baseline/contracts-v1.binpb`, fixture shape, executable Draft 2020-12 boundary
vectors, public-declaration documentation, generated TypeScript EOF
normalization, Go compilation, and TypeScript types. It does not invoke remote
generation. `just generate` invokes only the pinned remote plugins, normalizes
the tracked outputs, refreshes the manifest, and fails if the exact worktree
changes so the diff must be reviewed and committed. Neither path changes the
compatibility baseline.

Replacing `baseline/contracts-v1.binpb` is an orchestrator-owned single-writer
operation. It requires a tracked accepted compatibility decision and a
separately reviewed explicit `buf build` command; there is no environment flag
or package script that can refresh the baseline.

The generated bindings have local locked runtime dependencies. Check both
language projections after package installation:

```sh
go test ./gen/go/...
pnpm typecheck
```

## Errors, side effects, and costs

Package commands fail loudly for a missing/wrong tool version, schema
violation, breaking change, malformed fixture, missing generated output, or
source/output digest drift. Only generation contacts the Buf Schema Registry
to run pinned remote plugins. It can consume registry bandwidth and local cache
space but creates no Modal/VPS resource, authority state, or external product
effect. It sends no customer evidence: only checked-in `.proto` source enters
remote generation.

`tests/fixtures/boundary-cases.json` is a contract-vector set for missing,
empty, malformed, wrong-type, oversized, duplicate, concurrent, stale,
revoked, and principal-mismatch cases. It validates expected semantics and is
not a claim that a future gateway/runtime has enforced them; service owners must
execute the vectors as real boundary conformance tests when they add handlers.
`tools/verify-jsonschema.mjs` additionally compiles every public-edge generated
schema and executes positive and rejection vectors for nested required fields,
bounds, types, timestamps, and unknown properties.

## Provenance and cleanup

`tooling.yaml` records official source URLs, versions, commits, and licenses.
Remote plugin versions and BSR revisions are pinned in `buf.gen.yaml`; no local
`protoc` plugin is trusted. The generated projections are tracked source artifacts. Temporary
Buf cache data belongs outside this repository and should be removed only when
it was created for this task and is not shared by another active process.

Exact unverified product items: no authenticated Connect gateway service,
runtime handler, persistence adapter, or end-user client uses these contracts
yet. Those integrations begin at Stage 02 and retain this package as schema
authority.
