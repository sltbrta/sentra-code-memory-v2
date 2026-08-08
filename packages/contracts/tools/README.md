# Contract package tools

These scripts run deterministic local contract checks. They require exactly the
Buf version recorded in `../tooling.yaml`, use only tracked inputs/outputs, and
do not create runtime authority or cloud resources.

`bazel-verify.sh` copies the complete package into Bazel's private test root,
installs the frozen JavaScript dependency graph with the repository-pinned
Node/pnpm pair, and reruns schema, compatibility, fixture, generated-code shape,
and TypeScript gates without invoking remote code-generation plugins. The
facade may provide cleanup-bounded shared Buf and pnpm caches; pinned module and
lockfile integrity remain authoritative and package work stays inside Bazel's
private test root. It never writes to the checkout.

`bazel-generate.sh` is the explicit mutation boundary for `just generate`. It
accepts only the canonical Git worktree supplied by Bazel and exact Buf 1.71.0,
then updates the declared `gen/go`, `gen/jsonschema`, and `gen/ts` projections.
Malformed paths or tool versions fail with a static JSON error; no credentials,
staging, commits, or effects beyond the declared generated paths are performed.
The enclosing `just generate` command owns remote-plugin generation and fails
its receipt if generation changes the exact input tree, leaving the diff for
explicit review. `normalize-generated-go.rb` and `normalize-generated-ts.rb`
make the remote-plugin output deterministic under the repository import-order
and EOF gates; both are strict and abort on an unexpected shape.
