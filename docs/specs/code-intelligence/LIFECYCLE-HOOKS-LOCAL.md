<!-- markdownlint-disable MD060 MD040 -->

# Local Lifecycle Hooks (issue #59)

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
| `repo-hooks` (default)| `<root>/.sentra/hooks/<kind>`              | contained  |
| `git-common-hooks`    | `<git-common-dir>/hooks/<kind>`            | shared, explicit opt-in |

The `repo-hooks` strategy is the only safe default; the `git-common-hooks`
strategy is gated behind `--allow-unsafe-git-common=true` and refuses to
overwrite a non-sentra hook in the shared directory.

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
| `hooks_local`   | `uninstall`  | `action`, `root`                    | `strategy`, `kinds`                                     |
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
bump, no temp file, identical manifest).

## Rollback

Each successful install writes a JSON manifest at
`<root>/.sentra/state/sentra-manifest.json` (or
`<git-common>/hooks/state/sentra-manifest.json` for the shared strategy).
The manifest records:

- the SHA-256 of every installed hook script;
- the prior file path, contents, and mode of every hook (when one existed);
- the strategy and the resolved `core.hooksPath` (only for repo-hooks).

Uninstall consults the manifest and restores every prior file byte-for-byte
(matching mode and existence). When no manifest exists, uninstall is a no-op
with a friendly note rather than an error.

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
- CLI tests: `services/brain/cmd/sentra-code-memory/cli_local_test.go`
- Codeserve tests: `services/brain/internal/codeserve/local_handlers_test.go`
- No-network assertion: `TestNoNetworkImports` greps for `net/*` imports.
