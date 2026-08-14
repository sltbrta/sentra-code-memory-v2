<!-- markdownlint-disable MD060 MD040 -->

# Local Lifecycle Hooks (issue #59)

## Trust boundary (issue #63)

Installing, uninstalling, or running lifecycle hooks leaves persistent
host state behind (filesystem hook scripts under `<root>/.sentra/hooks/`
or `<git-common>/hooks/`, plus a local `core.hooksPath` flip for the
default strategy). The wire shape accepts an arbitrary `root` and
`cli_path`, which is necessary for the operator-driven direct CLI but
becomes an arbitrary-executable-persistence primitive the moment a
network/IPC surface is reachable by a model-controlled caller.

The contract encodes this asymmetry in `VerbSpec.RequiresOperatorTrust`
and keeps enforcement at the adapter layer so direct CLI and explicit
operator JSONL pipelines are unchanged:

| Surface                          | `install` / `uninstall` / `run` | `status` |
| -------------------------------- | --------------------------------- | --------- |
| Direct CLI (`hooks install ...`) | always allowed (operator trust)   | allowed   |
| Direct JSONL (`serve` pipe)      | always allowed (operator trust)   | allowed   |
| HTTP `/dispatch` (loopback)      | refused without opt-in            | allowed   |
| MCP `tools/call` (stdio)         | refused without opt-in            | allowed   |
| HTTP `/dispatch` (token-protected) | refused without opt-in          | allowed   |

The opt-in is explicit and easy to grep for:

- HTTP: set `X-Sentra-Operator-Trust: 1` header **or**
  `?operator_trust=1` query parameter (both forms accepted because some
  HTTP bridges can only set one).
- MCP: include `"_operator_trust": true` in the JSON `arguments` object.

A refusal produces the canonical structured envelope so callers can
branch on `error_code == "operator_trust_required"` without parsing the
human-readable message:

```json
{
  "ok": false,
  "verb": "hooks_local",
  "action": "install",
  "error": "operator trust required for hooks_local action=install on http; set the explicit operator opt-in (HTTP X-Sentra-Operator-Trust header or ?operator_trust=1 query; MCP arguments._operator_trust=true) or run the direct CLI",
  "error_code": "operator_trust_required",
  "product_owned": true,
  "trust_required": {
    "verb": "hooks_local",
    "action": "install",
    "surface": "http",
    "opt_in_field": "_operator_trust"
  }
}
```

`codeserve.Handle` itself never inspects the trust flag, so:

- The direct CLI (`sentra-code-memory hooks install ...`) and any
  operator pipeline that pipes JSONL into `serve` keep their existing
  semantics; both call `codeserve.Handle(ctx, req)` with no extra
  field and the request flows straight through to `lifecycle.Install`.
- The catalog metadata (`CatalogMetadata` / the `catalog` verb with
  `detail=true`) now carries
  `requires_operator_trust: true` on `hooks_local`, so any contract
  consumer (MCP client UI, catalog linter, audit script) can render
  the gate without consulting adapter-specific docs.
- Only the HTTP and MCP adapters are affected; the SCIP and dense
  paths are unrelated and untouched.

The split is deliberate: it preserves the local-first opt-in the issue
\#59 spec demanded while ensuring a model-controlled process can only
invoke the read-only `status` action over the wire by default. An
operator who really wants model-driven lifecycle work sets the opt-in
explicitly and accepts the persistence implications in code review.

## Scope and non-goals

This document captures the local-first hook lifecycle added in response to
issue #59 ("Evaluate local lifecycle hooks and dense retrieval separately").
It is a deliberately bounded surface that does **not** ship the SCM-runtime
`lifecycle_install` verb deferred by ADR 0025. The deferred verb covers a
managed server install/service/uninstall lifecycle; this surface covers a
repo-confined, agent-facing git hook installer that is opt-in, atomic,
idempotent, and rollback-safe.

This surface never imports a network package, never installs hooks outside
the user's own checkout, and never overwrites a hook owned by another tool
when the install target is the shared git common directory.

## Confinement

| Strategy              | Writes to                                  | Risk class |
| --------------------- | ------------------------------------------ | ---------- |
| `repo-hooks` (default)| `<root>/.sentra/hooks/<kind>` plus the local `core.hooksPath` git setting | contained  |
| `git-common-hooks`    | `<git-common-dir>/hooks/<kind>`            | shared, explicit opt-in |

The `repo-hooks` strategy is the only safe default; its only write inside
`.git/` is the local `core.hooksPath` setting (no hook files are ever
written there). The `git-common-hooks` strategy is gated behind
`--allow-unsafe-git-common=true` and refuses to overwrite a non-sentra hook
in the shared directory.

## API surface

### CLI

```
sentra-code-memory hooks [--root PATH] [--strategy NAME] [--kinds KINDS] \
                          [--cli-path PATH] [--event NAME] \
                          [--allow-unsafe-git-common] \
                          (install|status|uninstall|run)
```

The action may appear before or after the flags; both forms are accepted.
`--event` is only meaningful for the `run` action.

### JSONL

| Verb            | Action       | Required                            | Optional                                                |
| --------------- | ------------ | ----------------------------------- | ------------------------------------------------------- |
| `hooks_local`   | `install`    | `action`                            | `root`, `strategy`, `kinds`, `allow_unsafe_git_common`, `cli_path` |
| `hooks_local`   | `status`     | `action`, `root`                    | —                                                       |
| `hooks_local`   | `uninstall`  | `action`, `root`                    | `strategy` (`kinds` is accepted but ignored: uninstall always restores every recorded hook) |
| `hooks_local`   | `run`        | `action`, `event`                   | `root`                                                  |

The `run` action is the entry point installed hook scripts call. It is a
no-op by design: lifecycle events are observable through the existing
code-watch pipeline, and a hook script never blocks a user commit even if
the CLI is misbehaving.

## Atomicity and idempotency

Every script write uses the standard temp-file + fsync + rename + parent
fsync dance:

1. Render the script body into a temp file in the destination directory.
2. fsync the temp file's bytes.
3. rename over the target path. A failed rename leaves the prior file
   intact.
4. fsync the parent directory so the rename is durable across a crash.

An interrupted install at any step leaves the previously installed hook
visible. Re-running the install with identical content is a no-op (no mtime
bump, no temp file, identical manifest including the original timestamp).

Sequential subset installs accumulate: the manifest carries forward every
entry from the previous install that the current run does not rewrite, so
`install --kinds post-commit` followed by `install --kinds post-merge`
leaves both hooks tracked and restorable. Reinstalling a hook with different
content (for example a new `--cli-path`) inherits the original
pre-first-install snapshot instead of snapshotting the installer's own
earlier script.

## Coexistence with existing hooks

The `repo-hooks` strategy flips `core.hooksPath`, which shadows the hooks
git would otherwise run. The installer never silently disables an existing
hook:

1. Before any write it records the prior local `core.hooksPath` value and
   resolves the prior hooks directory (the prior value, or the inherited
   value from global/system config, or `<git-common-dir>/hooks`).
2. For a hook kind the installer manages, the installed script delegates to
   the prior hook script after the sentra event and propagates its exit
   status, so gates like `pre-push` still veto when they used to.
3. Every other active hook in the prior hooks directory gets a
   sentinel-tagged passthrough script that execs the original, so hook kinds
   the installer does not manage keep running unchanged.
4. Uninstall restores the prior local `core.hooksPath` value verbatim (or
   unsets the setting when there was none), and only when the live value
   still matches the one the installer wrote.
5. Uninstall never removes or overwrites a hook file that is no longer
   sentinel-tagged sentra-managed; such files are skipped with a note.

A scan error on the prior hooks directory fails the install closed.

## Rollback

Each successful install writes a JSON manifest at
`<root>/.sentra/state/sentra-manifest.json` (or
`<git-common>/hooks/state/sentra-manifest.json` for the shared strategy).
The manifest records:

- the SHA-256 of every installed hook script;
- the prior file path, contents, and mode of every hook (when one existed);
- the strategy and the resolved `core.hooksPath` (only for repo-hooks);
- the prior `core.hooksPath` value and the prior hooks directory (only for
  repo-hooks), so uninstall restores the exact pre-install git state.

Uninstall consults the manifest and restores every prior file byte-for-byte
(matching mode and existence), removes hooks that had no prior file,
restores or clears `core.hooksPath`, and skips any file that is no longer
sentra-managed. When no manifest exists, uninstall is a no-op with a
friendly note rather than an error.

## What is not implemented

The deferred verbs in the codeserve catalog (`lifecycle_install`,
`code_dense_rerank`, `session_product`, `hosted_tenancy`,
`query_advanced`) remain deferred per ADR 0025. Calling them returns a
structured `error_code: "deferred"` disclosure.

The `git-common-hooks` strategy refuses any non-sentra hook. To overwrite a
hook owned by another tool, the user must remove that hook first and only
then run the installer.

## Evidence

- Unit tests: `services/brain/internal/lifecycle/lifecycle_test.go`
- Regression tests (pre-existing hooksPath, existing-hook delegation,
  sequential subset installs):
  `services/brain/internal/lifecycle/lifecycle_regression_test.go`
- CLI tests: `services/brain/cmd/sentra-code-memory/cli_local_test.go`
- Codeserve tests: `services/brain/internal/codeserve/local_handlers_test.go`
- No-network assertion: `TestNoNetworkImports` greps for `net/*` imports.
