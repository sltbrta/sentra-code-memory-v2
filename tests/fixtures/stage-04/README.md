# Stage 04 grounding fixture

`grounding/query-cases.json` freezes twelve deterministic query cases over the
Stage 03 mixed-P5 committed fixture: three answered, two partial, two absent,
two stale, two denied, and one provider-failure case. Each case declares its
pinned generation (`current` is the reconciled generation 2, `stale` is the
admitted generation 1), freshness mode, interference, expected answer status,
exact expected citation ranges, and permitted abstention reason set.

Expected citation paths resolve to the Stage 03 delta-manifest trees, and every
cited range is bounded by the deterministic generated file content (seed lines
plus the fixture comment line). The Stage 04 contract conformance test verifies
that shape without implementing retrieval or synthesis.

Two frozen non-disclosure rules anchor here: denied support and revoked-mid-query
support must surface exactly the `absent_support` reason, identical to genuinely
absent support; a failed provider must surface only `synthesis_unavailable`.

This fixture proves no answer quality. Product behavior arrives with the Stage
04 leaves; the integration wave extends this manifest toward the packet's
sixty-case depth without changing these twelve frozen cases.
