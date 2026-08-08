# Factory sealed runner

Package `runner` is the Stage 05 sealed runner boundary. It executes exactly
one bounded, leased leaf against an isolated exact-base candidate and fails
closed on any escape.

## Boundary

Per decision-review issue #22, runner isolation lives here in factory/broker,
never in `apps/tui`. The leaf receives only the sealed `Effects` surface —
brokered edit proposal, brokered exact-base reads, and a brokered generic
effect valve. No dispatch, task-creation, shell, network, Git, model,
credential, plugin, or instruction authority exists on that surface at all;
requesting any of those denies, records an `escape_*` denial in the trace,
and fails the run closed with the candidate discarded and a rollback receipt.

Runner output is trace only: canonical state changes flow through the kernel
with fence checks, and the canonical worktree plus complete `.git` inventory
stay byte-identical across success and every failure.

## Execution

`RunLeaf` validates the leaf spec (grant validity, no dispatch authority,
base binding), reauthorizes admission under current policy, opens the
exact-base candidate, runs the `Synthesizer` against the sealed surface, and
atomically applies the staged edits through `gitcandidate.Store.Apply` with
the effect broker reauthorizing every mutation at mutation time. Reads and
effect requests reauthorize against the live broker clock, so a grant or
lease expiring mid-run denies. A stale lease or fence mid-apply, any edit
failure (including set-level violations), any recorded denial, or any
synthesizer error discards the whole candidate with zero residue and a
rollback receipt; the escape reason is used only for `escape_*` denials,
every synthesizer error is recorded in the denial trace, and rollback
receipts bind the staged edit set (or the canonical empty digest when
nothing was staged). An exact replay of a rejected application replays the
rejection — the run never flips to completed.

## Synthesizer port

`Synthesizer` is the integration seam. v1 executes the deterministic
fixture-driven synthesizer (`ScriptedSynthesizer`/`FixtureSynthesizer`) in
process; model-provider execution plugs in later against the same sealed
`Effects` surface and inherits the identical broker, candidate, and failure
semantics.

Acceptance label: `//services/broker/internal/factory/runner:runner_test`.
