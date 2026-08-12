# contextpack

Deterministic, bounded context packing for agent-facing retrieval payloads
(Phase 1 vertical slice: issues #7–#11).

## Role

Given scored candidate sources, `Pack` produces a byte/token-bounded context
result with explicit accounting:

- **Hard budgets** (`Budget{MaxBytes, MaxTokens}`) — the stricter of the two
  binds; tokens are estimated deterministically at 4 bytes/token. (#7)
- **Relevance-proportional allocation** with a **direct-source floor**
  (default 40% of the budget split evenly among `Direct` sources before the
  remainder is distributed proportional to score). (#7)
- **Render modes** — `full`, `signatures` (declarations only), `skeleton`
  (declarations + structural closers), `compact` (comments/blank lines
  stripped). (#10)
- **No silent drops** — every truncated item is marked, every omitted item
  carries a reason and a stable expansion `Handle`
  (`h_<hash of path|range|fingerprint>`). Content drift changes the handle ID;
  `Registry.Resolve` classifies handles as `ok`, `stale`, or `unknown`. (#8)
- **Session dedup** — `Registry.Track` keys on (path, range, content
  fingerprint); repeated unchanged source returns a `d<ordinal>` back-pointer
  and emits zero duplicate bytes. (#9)
- **Resource governor** — `Limits{MaxWorkers, MaxCandidates, MaxOutputBytes,
  MaxWallTime}` enforced fail-safe by `Governor` (deny on overflow, never
  panic), with a visible `Report` in result metadata. Wall time uses an
  injectable clock for deterministic tests. (#11)

## Determinism

Packing is a pure function of its inputs: stable sort by (score desc, path,
start line), integer budget arithmetic, no maps in output order, no wall
clock outside the injected governor time source. Identical inputs pack to
byte-identical results.

## Non-goals

No external dependencies, no daemon, no persistence — session state lives in
process memory and callers own `Registry` lifetime. `codeserve` wires this
into `code_find_relevant` behind opt-in `max_bytes` / `max_tokens` / `render`
/ `session` request fields; default behavior is unchanged.

## Tests

`go test ./services/brain/internal/contextpack/`
