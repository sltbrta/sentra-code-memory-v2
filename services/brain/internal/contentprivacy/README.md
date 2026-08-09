# Content privacy boundary

`contentprivacy` is a reusable in-process boundary for turning already-extracted
text into query-facing projections. `ProductionProjectionAdapter` is the
explicit validated-publish-before-commit composition seam: it requires both a
`Guard` and a `ProjectionPublisher`, fails closed when either is absent, and
never sends a quarantined or tombstoned decision to the sink. A failed or
panicking publisher leaves no admission receipt or readable record, so the same
stable scoped content identity can be retried safely; sinks must use that
identity idempotently because an error can leave external publication uncertain.
This repository does not yet bind that adapter to a deployed policy, durable
receipt store, or hosted sink, so it makes no production-coverage claim. The
package does **not** parse source objects, own tenant/group membership, or
replace the ArtifactVault or evidence authorities.

## Exact owned boundary

- Select one explicit immutable policy by `individual`, `team`, or `company`
  scope. Missing/malformed scope policy fails closed.
- Run dependency-free deterministic regex detectors only for configured PII
  (`email`, `phone`, `ssn`, `credit_card`) and secret (`api_key`,
  `bearer_token`, `private_key`, `password_assignment`) classes, within a hard
  1 MiB/item and 1,024-finding ceiling and smaller required policy limits.
- Apply strongest-wins disposition: `tombstone > quarantine > redact`. Detector
  errors, malformed detector spans, or byte offsets that split a UTF-8 rune use
  the policy's explicit fail-closed `quarantine` or `tombstone` action; they can
  never publish.
- Emit content-free receipts containing the canonical policy ID, version, and
  digest. Receipts retain IDs/classes/status, never matched values.
- Construct every publishable surface inside the guard's shared validated
  admission path: `IndexText` and `CacheText` are derived only from sanitized
  `Content`, and claims are detected and sanitized independently. Callers
  cannot supply alternate cache/index text.
- Drop citations whose ranges overlap any detected content span. Retained
  citations keep stable byte offsets and regenerate quotes only from wholly
  non-sensitive ranges, giving a zero citation-to-redacted-span invariant.
- Keep admitted originals only in the guard's bounded in-memory adapter for
  retention and exceptional reveal. Reveal requires policy opt-in, a non-nil
  live `RevealAuthorizer`, principal, and reason on every call.
- Enforce retention on reads and sweeps. A sweep timestamp later than the
  guard's trusted clock fails closed; retention receipts and tombstones are
  stamped from that trusted clock rather than caller time. Expiry and explicit
  deletion remove raw/projection data and retain an authoritative tombstone
  that blocks re-admission of the scoped ID.
- Carry the opaque `Blind` bit unchanged. The API has no benchmark-gold field;
  detectors and dispositions cannot consume gold labels.

## Deterministic offline evaluation

`Evaluate` accepts explicitly offline `EvaluationCase` values. Gold spans are
kept in `EvaluationCase.ExpectedFindings`; they are not fields on `Input`,
`Projection`, or `Receipt`, and the returned aggregate contains no content,
matched values, tenant IDs, content IDs, or scope IDs. Cases must use unique
scoped content IDs.

The aggregate reports exact integer counts and a finite rate for each metric;
`0/0` is represented as a zero rate while remaining distinguishable by its zero
denominator:

- `Precision`: exact class + surface + byte-range true positives divided by all
  distinct detector findings.
- `Recall`: exact matches divided by labeled spans.
- `FalseRedactionRate`: unique published masked bytes outside every labeled
  sensitive span divided by all unique published masked bytes.
- `DetectorCoverage`: expected classes for which every labeled span was found,
  divided by distinct expected classes. This is deliberately stricter than
  merely invoking a configured detector.
- `DeletionCorrectness`: requested evaluation deletions that commit a
  tombstone, remove the retained raw/projection record, deny the projection,
  and block re-admission, divided by requested evaluation deletions.
- `CitationToRedactedSpanRate`: retained citations overlapping a detected
  content span divided by retained citations. The required invariant is zero;
  a case with no retained citation reports `0/0`, not evidence of coverage.

These are deterministic component metrics over caller-supplied fixtures. They
are not live telemetry, corpus representativeness, universal detector quality,
or a production rollout claim.

## Deliberate non-claims

- The local regex set is a testable baseline, not universal DLP, OCR, NER,
  entropy detection, format-preserving tokenization, or regulatory
certification.
- The in-memory guard is not durable encrypted storage, KMS, legal hold,
  production audit persistence, SCIM/OpenFGA authority, or a cross-store purge
  coordinator. Composition must persist policy receipts/tombstones and keep raw
  bytes in the existing authorized vault boundary.
- The production adapter proves fail-closed validated-publish-before-commit
  composition in process. It does not prove that a deployed ingest, cache,
  index, or query path constructs it; deployment must bind a policy, guard,
  publisher, receipt and tombstone persistence, and existing ACL authority
  before claiming coverage.

Focused acceptance targets:

```text
cd services && go test ./brain/internal/contentprivacy/... -count=1
cd services && go test -race ./brain/internal/contentprivacy/... -count=1
bazel test //services/brain/internal/contentprivacy:contentprivacy_test
```
