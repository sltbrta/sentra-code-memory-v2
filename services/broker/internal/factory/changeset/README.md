# Factory changeset

Package `changeset` validates the frozen Stage 05 atomic candidate edit set.
It mirrors the `ChangeSetPreview`/`PreviewEdit` invariants of
`packages/contracts/proto/ouroboros/contracts/v1/factory.proto` and
`docs/stages/stage-05/SPEC-DELTA-001.md`: normalized repository-relative
paths, unique post-image and pre-image paths, the exact per-operation
before/after digest shapes (add: after only; delete: before only; modify and
rename: both), rename-only old paths, the bounded P5 language vocabulary, and
after-digest equality with the carried post-image bytes.

The package is pure: no filesystem, Git, or time access, and a single static
`ErrInvalid` for every rejection so validation detail never becomes a
disclosing oracle. `DigestBytes` is the canonical sha256 content binding used
across the factory leaf.

Acceptance label: `//services/broker/internal/factory/changeset:changeset_test`.
