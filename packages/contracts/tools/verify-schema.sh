#!/bin/sh
# Verify schema invariants usable from a Bazel runfiles tree.
#
# Generated-drift verification stays in verify.sh because it requires the exact
# source checkout and Git tree rather than a sandboxed runfiles projection.
set -eu

package_dir=$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd -P)
expected_version=1.71.0

if ! command -v buf >/dev/null 2>&1; then
	printf '%s\n' 'OURO-CONTRACT-BUF-MISSING: install Buf 1.71.0.' >&2
	exit 127
fi

actual_version=$(buf --version)
if [ "$actual_version" != "$expected_version" ]; then
	printf '%s\n' "OURO-CONTRACT-BUF-VERSION: expected $expected_version, got $actual_version." >&2
	exit 2
fi

cd "$package_dir"
buf format --diff --exit-code --config buf.yaml proto
buf lint --config buf.yaml proto
buf build --config buf.yaml proto
buf breaking --config buf.yaml proto --against baseline/contracts-v1.binpb
"$package_dir/tools/verify-authority-contract.rb"
"$package_dir/tools/verify-fixtures.rb" "$package_dir/tests/fixtures/boundary-cases.json"
"$package_dir/tools/test-doc-coverage.rb"
"$package_dir/tools/check-doc-coverage.rb" --config "$package_dir/buf.yaml" "$package_dir/proto"
"$package_dir/tools/check-generated-eof.rb" "$package_dir/gen/ts"
