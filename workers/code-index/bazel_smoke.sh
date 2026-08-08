#!/usr/bin/env bash
# Run the dependency-free local worker boundary under Bazel's isolated test dir.
# Issue #19 owns the root Rust rules and toolchain registration. Until that
# lands, the package adapter accepts only an explicit pinned Cargo executable.

set -euo pipefail

: "${TEST_SRCDIR:?Bazel must provide TEST_SRCDIR}"
: "${TEST_WORKSPACE:?Bazel must provide TEST_WORKSPACE}"
: "${TEST_TMPDIR:?Bazel must provide TEST_TMPDIR}"

workspace_root="${TEST_SRCDIR}/${TEST_WORKSPACE}/workers/code-index"
if [[ ! -d "${workspace_root}" ]]; then
	printf '%s\n' 'OURO-STAGE1-BAZEL-RUNFILES-MISSING' >&2
	exit 2
fi

toolchain_error='OURO-STAGE1-PINNED-RUST-TOOLCHAIN-MISSING'
cargo_binary="${OUROBOROS_STAGE1_CARGO:-}"
if [[ "${cargo_binary}" != /* || ! -x "${cargo_binary}" ]]; then
	printf '%s\n' "${toolchain_error}" >&2
	exit 2
fi
toolchain_directory="$(dirname "${cargo_binary}")"
rustc_binary="${toolchain_directory}/rustc"
if [[ ! -x "${rustc_binary}" ]]; then
	printf '%s\n' "${toolchain_error}" >&2
	exit 2
fi
if ! cargo_version="$("${cargo_binary}" --version 2>/dev/null)" || [[ "${cargo_version}" != "cargo 1.95.0 "* ]]; then
	printf '%s\n' "${toolchain_error}" >&2
	exit 2
fi
if ! rustc_version="$("${rustc_binary}" --version 2>/dev/null)" || [[ "${rustc_version}" != "rustc 1.95.0 "* ]]; then
	printf '%s\n' "${toolchain_error}" >&2
	exit 2
fi

for variable in \
	AWS_ACCESS_KEY_ID AWS_SECRET_ACCESS_KEY AWS_SESSION_TOKEN \
	CARGO_REGISTRIES_CRATES_IO_TOKEN GITHUB_TOKEN GIT_CONFIG_GLOBAL \
	HTTPS_PROXY HTTP_PROXY ALL_PROXY https_proxy http_proxy all_proxy \
	RUSTUP_HOME RUSTUP_TOOLCHAIN; do
	unset "${variable}" || true
done

export CARGO_NET_OFFLINE=true
export CARGO_HOME="${TEST_TMPDIR}/cargo-home"
export CARGO_TARGET_DIR="${TEST_TMPDIR}/cargo-target"
export HOME="${TEST_TMPDIR}/home"
export PATH="${toolchain_directory}:/usr/bin:/bin"
mkdir -p "${CARGO_HOME}" "${CARGO_TARGET_DIR}" "${HOME}"

cd "${workspace_root}"
"${cargo_binary}" test --locked --offline
