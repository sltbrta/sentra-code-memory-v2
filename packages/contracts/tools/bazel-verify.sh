#!/bin/sh
# Verify the complete contract package from an isolated Bazel test projection.
#
# Dependencies install into TEST_TMPDIR while integrity-checked download caches
# may be shared explicitly. Verification is read-only, never invokes remote
# generation plugins, and never writes to the source checkout.

set -eu
umask 077

fail_toolchain() {
	printf '%s\n' '{"code":"OURO-CONTRACT-TOOLCHAIN-UNAVAILABLE","message":"Declare absolute pinned Buf, Node, and pnpm executables."}' >&2
	exit 78
}

: "${TEST_SRCDIR:?Bazel must provide TEST_SRCDIR}"
: "${TEST_WORKSPACE:?Bazel must provide TEST_WORKSPACE}"
: "${TEST_TMPDIR:?Bazel must provide TEST_TMPDIR}"
case "${OUROBOROS_BUF_BIN:-}:${OUROBOROS_NODE_BIN:-}:${OUROBOROS_PNPM_BIN:-}" in
/*:/*:/*) ;;
*) fail_toolchain ;;
esac
for executable in "$OUROBOROS_BUF_BIN" "$OUROBOROS_NODE_BIN" "$OUROBOROS_PNPM_BIN"; do
	[ -f "$executable" ] && [ -x "$executable" ] || fail_toolchain
done
[ "$("$OUROBOROS_BUF_BIN" --version 2>/dev/null)" = '1.71.0' ] || fail_toolchain
case "$("$OUROBOROS_NODE_BIN" --version 2>/dev/null)" in
v2[0-9].* | v[3-9][0-9].*) ;;
*) fail_toolchain ;;
esac
[ "$("$OUROBOROS_NODE_BIN" "$OUROBOROS_PNPM_BIN" --version 2>/dev/null)" = '10.18.0' ] || fail_toolchain

source_root="${TEST_SRCDIR}/${TEST_WORKSPACE}/packages/contracts"
[ -d "$source_root" ] || fail_toolchain
work_root="${TEST_TMPDIR}/contracts"
home_root="${TEST_TMPDIR}/home"
case "${OUROBOROS_PNPM_STORE:-}" in
"") store_root="${TEST_TMPDIR}/pnpm-store" ;;
/*) store_root=$OUROBOROS_PNPM_STORE ;;
*) fail_toolchain ;;
esac
case "${OUROBOROS_BUF_CACHE:-}" in
"") buf_cache="${TEST_TMPDIR}/buf-cache" ;;
/*) buf_cache=$OUROBOROS_BUF_CACHE ;;
*) fail_toolchain ;;
esac
mkdir -p "$work_root" "$home_root" "$store_root" "$buf_cache"
cp -RL "$source_root/." "$work_root/"

export HOME="$home_root"
export XDG_CACHE_HOME="${TEST_TMPDIR}/xdg-cache"
export XDG_CONFIG_HOME="${TEST_TMPDIR}/xdg-config"
buf_directory=$(dirname -- "$OUROBOROS_BUF_BIN")
node_directory=$(dirname -- "$OUROBOROS_NODE_BIN")
PATH="${buf_directory}:${node_directory}:/usr/bin:/bin"
export PATH
export RUBYOPT='-EUTF-8:UTF-8'
export BUF_CACHE_DIR="$buf_cache"
unset GITHUB_TOKEN NPM_TOKEN NODE_AUTH_TOKEN BUF_TOKEN

cd "$work_root"
"$work_root/tools/generated-manifest.rb" check
manifest_probe="$work_root/proto/ouroboros/contracts/v1/common.proto"
manifest_probe_backup="${TEST_TMPDIR}/common.proto"
cp "$manifest_probe" "$manifest_probe_backup"
printf '\n' >>"$manifest_probe"
if manifest_output=$("$work_root/tools/generated-manifest.rb" check 2>&1); then
	printf '%s\n' '{"code":"OURO-CONTRACT-GENERATED-DRIFT-FALSE-PASS","message":"The generated manifest accepted a changed source."}' >&2
	exit 1
fi
printf '%s' "$manifest_output" | grep -F 'OURO-CONTRACT-GENERATED-DRIFT' >/dev/null || {
	printf '%s\n' '{"code":"OURO-CONTRACT-GENERATED-DRIFT-ERROR-INVALID","message":"The generated manifest returned an unexpected error."}' >&2
	exit 1
}
mv "$manifest_probe_backup" "$manifest_probe"
"$work_root/tools/generated-manifest.rb" check >/dev/null
for generated_probe in \
	"$work_root/gen/go/ouroboros/contracts/v1/contractsv1connect/local_authority.connect.go" \
	"$work_root/gen/go/ouroboros/contracts/v1/BUILD.bazel"; do
	generated_probe_backup="${TEST_TMPDIR}/generated-output.probe"
	cp "$generated_probe" "$generated_probe_backup"
	printf '\n' >>"$generated_probe"
	if manifest_output=$("$work_root/tools/generated-manifest.rb" check 2>&1); then
		printf '%s\n' '{"code":"OURO-CONTRACT-OUTPUT-DRIFT-FALSE-PASS","message":"The generated manifest accepted a changed output."}' >&2
		exit 1
	fi
	printf '%s' "$manifest_output" | grep -F 'OURO-CONTRACT-GENERATED-DRIFT' >/dev/null || {
		printf '%s\n' '{"code":"OURO-CONTRACT-OUTPUT-DRIFT-ERROR-INVALID","message":"The generated manifest returned an unexpected output error."}' >&2
		exit 1
	}
	mv "$generated_probe_backup" "$generated_probe"
	"$work_root/tools/generated-manifest.rb" check >/dev/null
done
"$OUROBOROS_NODE_BIN" "$OUROBOROS_PNPM_BIN" install \
	--frozen-lockfile \
	--ignore-scripts \
	--store-dir "$store_root" \
	--reporter=silent
"$work_root/tools/verify-schema.sh"
"$OUROBOROS_NODE_BIN" "$work_root/tools/verify-jsonschema.mjs"
"$OUROBOROS_NODE_BIN" "$OUROBOROS_PNPM_BIN" exec tsc --noEmit --project tsconfig.json
printf '%s\n' '{"schema_version":"stage1.contracts.verify.v1","status":"ok"}'
