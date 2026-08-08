# Stage 05 factory fixture

`factory/factory-cases.json` freezes nine deterministic cases over the Stage 03
mixed-P5 committed fixture's Go lane: one retained happy path plus the eight
acceptance-matrix outcomes — stale base, stale lease, duplicate message, leaf
escape attempt, partial edit failure, failed gate, revoke, and rollback. Each
case declares its intent scope, disjoint leaf write scopes, interference,
expected outcome, public reason, run and candidate states, gate roster, and
per-language obligations.

Three invariants anchor here and hold in every case: the public rejection
reason is always exactly `not_found_or_denied` (no existence detail leaks);
the canonical repository stays byte-identical in every outcome because
candidates live only in the isolated exact-base store; and a rejected
candidate always carries its rollback receipt.

This fixture proves no factory behavior. Product behavior arrives with the
Stage 05 leaves; the integration wave extends this manifest toward the
packet's full fixture depth without changing these nine frozen cases. Scopes
may reference rename pre-image paths, which resolve against the committed
tree.
