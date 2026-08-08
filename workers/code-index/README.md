# Code-index worker boundary

Status: **[planned] — Stage 1 deterministic smoke boundary, not an indexer.**

`ouroboros-code-index` validates a deliberately narrow JSON request and emits a
canonical JSON receipt. It proves that native Cargo and package-local Bazel
entry points can run a deterministic worker boundary without repository reads,
network access, credentials, plugins, compiler/LSP execution, or code indexing.
It is not the future canonical receipt schema; Issue #15 owns that contract.

## Local contract

The complete local grammar is:

```text
request      = ws "{" ws member ws "," ws member ws "}" ws
member       = string-key ws ":" ws ascii-string
string-key   = DQUOTE ("input" | "config") DQUOTE
ascii-string = DQUOTE 1*safe-ascii DQUOTE
ws           = *(SP | HTAB | LF | CR)
```

Each key appears exactly once in either order. `ascii-string` is a non-empty,
double-quoted sequence of bytes `0x20`–`0x7e` excluding quote and backslash;
each value is at most 4 KiB and the full request at most 8 KiB. Raw backslashes
are rejected before parsing, so escaped keys, values, newlines, quotes, and
Unicode are all outside this Stage 1 grammar. Whitespace is JSON whitespace
outside strings only.

For example:

```json
{"input":"evidence","config":"stage1-config"}
```

The receipt is newline-terminated canonical JSON, with fixed `schema_version`,
`runtime`, `operation`, and `status`, and SHA-256 digests of the supplied input
and configuration. Errors use static `OURO-STAGE1-*` codes and never echo the
payload or operating-system error text. Valid top-level JSON values other than
objects are classified as invalid JSON at this restricted boundary. If standard
error is closed or unwritable, the process exits with status 2 without retrying
or writing a fallback message.

[`tests/receipt_matrix.tsv`](tests/receipt_matrix.tsv) and
[`tests/expected_receipt.json`](tests/expected_receipt.json) are the one tracked
cross-runtime request/outcome fixture. Rust unit tests, Python tests, and the
cross-runtime CLI test all consume those exact bytes.

## Commands

Run these from this directory. They are local-only and need no network; Cargo
is invoked with `--locked --offline` where dependency resolution is relevant.

```sh
cargo fmt --check
cargo clippy --all-targets --all-features -- -D warnings
cargo test --locked --offline
printf '%s' '{"input":"evidence","config":"stage1-config"}' \
  | cargo run --locked --offline --quiet
```

The `stage1_smoke` Bazel target requires `OUROBOROS_STAGE1_CARGO` to name an
absolute executable Cargo path from a pinned Rust 1.95.0 toolchain whose same
directory contains Rustc 1.95.0. Missing, unpinned, or mismatched tools fail
with `OURO-STAGE1-PINNED-RUST-TOOLCHAIN-MISSING`; the adapter never searches
ambient `PATH`, rustup state, or `HOME`. It then runs the Cargo suite in Bazel's
test scratch directory with a fresh `HOME`, `CARGO_HOME`, and
`CARGO_TARGET_DIR`, Cargo networking disabled, and common credential/proxy
variables cleared.

This explicit executable contract is a fail-closed bridge, not a claim of
hermetic Bazel provisioning. Issue #19 must register the root Rust 1.95.0
toolchain, replace the bridge with `rules_rust` targets or a toolchain-aware
launcher, and make the compiler executable a declared Bazel tool input before
the target can run in remote execution.

```sh
OUROBOROS_STAGE1_CARGO="$(rustup which cargo)" \
  bazel test --test_env=OUROBOROS_STAGE1_CARGO \
  //workers/code-index:stage1_smoke
```

## Limits and cleanup

Requests are capped at 8 KiB and each field at 4 KiB. The worker has no
background processes, files, databases, queues, network calls, or cloud
resources. Native Cargo artifacts (`target/`) and Bazel test outputs are
reproducible and must be removed after verification.
