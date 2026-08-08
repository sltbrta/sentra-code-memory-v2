#!/bin/sh
# Verify schema shape, compatibility, documentation, fixtures, and generated drift.
set -eu

package_dir=$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd -P)

"$package_dir/tools/verify-schema.sh"
node "$package_dir/tools/verify-jsonschema.mjs"
cd "$package_dir"
"$package_dir/tools/check-generated.sh"
