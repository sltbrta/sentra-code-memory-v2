# Stage 06 tracer fixtures

`tracer/manifest.json` is the **synthetic hermetic** pin for Tracer 001
(`pinStatus: pinned`). Digests are content-addressed from committed source,
issue-bundle, and identity-tuple bytes. This is **not** a live Sentra/SFS
reconstruction claim — that pin remains deferred (see
`docs/stages/stage-06/provenance/SYNTHETIC-FIXTURE.md` and SPEC-DELTA-001).

| Path | Role |
| --- | --- |
| `tracer/manifest.json` | Pinned identity tuple + five variants |
| `tracer/source/` | Bounded Go supporting-span tree |
| `tracer/issue-bundle.json` | Synthetic issue-evidence bundle |
| `tracer/change-intent.json` | Factory handoff facts (exact base OID, scope, N) |
| `tracer/variants/` | Per-arm actor/ACL/source expectations |

Live dual-proof (Modal, GitHub draft PR, model keys) is out of this fixture
package; synthetic satisfies the deterministic **replay** half only.
