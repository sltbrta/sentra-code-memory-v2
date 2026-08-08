#!/bin/sh
# Generate tracked contract projections with the pinned Buf template.
#
# This script writes only the explicit gen/ outputs. It fails before generation
# when the caller supplies a different Buf version, preventing silent drift.
set -eu

package_dir=$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd -P)
expected_version=1.71.0

if ! command -v buf >/dev/null 2>&1; then
	printf '%s\n' 'OURO-CONTRACT-BUF-MISSING: install Buf 1.71.0.' >&2
	exit 127
fi
if ! command -v install >/dev/null 2>&1; then
	printf '%s\n' 'OURO-CONTRACT-INSTALL-MISSING: install a POSIX-compatible install command.' >&2
	exit 127
fi

actual_version=$(buf --version)
if [ "$actual_version" != "$expected_version" ]; then
	printf '%s\n' "OURO-CONTRACT-BUF-VERSION: expected $expected_version, got $actual_version." >&2
	exit 2
fi

cd "$package_dir"
buf generate --template buf.gen.yaml proto
"$package_dir/tools/normalize-generated-go.rb" "$package_dir/gen/go"
"$package_dir/tools/normalize-generated-ts.rb" "$package_dir/gen/ts"
install -m 0644 \
	"$package_dir/tools/templates/go-contracts-v1.BUILD.bazel" \
	"$package_dir/gen/go/ouroboros/contracts/v1/BUILD.bazel"
"$package_dir/tools/generated-manifest.rb" write
