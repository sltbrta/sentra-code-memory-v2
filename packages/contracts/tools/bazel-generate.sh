#!/bin/sh
# Regenerate tracked contract projections from an explicit Bazel run boundary.

set -eu
umask 077

fail() {
	printf '%s\n' '{"code":"OURO-CONTRACT-GENERATE-BOUNDARY-INVALID","message":"Generation requires the canonical checkout and pinned Buf 1.71.0."}' >&2
	exit 78
}

case "${BUILD_WORKSPACE_DIRECTORY:-}:${OUROBOROS_BUF_BIN:-}" in
/*:/*) ;;
*) fail ;;
esac
if [ ! -d "$BUILD_WORKSPACE_DIRECTORY/.git" ] && [ ! -f "$BUILD_WORKSPACE_DIRECTORY/.git" ]; then
	fail
fi
[ -f "$OUROBOROS_BUF_BIN" ] && [ -x "$OUROBOROS_BUF_BIN" ] || fail
[ "$("$OUROBOROS_BUF_BIN" --version 2>/dev/null)" = '1.71.0' ] || fail
package_root="$BUILD_WORKSPACE_DIRECTORY/packages/contracts"
[ -d "$package_root/proto" ] && [ -f "$package_root/buf.gen.yaml" ] || fail
case "${OUROBOROS_BUF_CACHE:-}" in
"") ;;
/*)
	mkdir -p "$OUROBOROS_BUF_CACHE"
	export BUF_CACHE_DIR="$OUROBOROS_BUF_CACHE"
	;;
*) fail ;;
esac

buf_directory=$(dirname -- "$OUROBOROS_BUF_BIN")
PATH="${buf_directory}:/usr/bin:/bin"
export PATH
unset BUF_TOKEN GITHUB_TOKEN
"$package_root/tools/generate.sh"
printf '%s\n' '{"schema_version":"stage1.contracts.generate.v1","generator":"buf:1.71.0","inputs":["packages/contracts/proto","packages/contracts/buf.gen.yaml"],"outputs":["packages/contracts/gen/go","packages/contracts/gen/jsonschema","packages/contracts/gen/ts"],"status":"ok"}'
