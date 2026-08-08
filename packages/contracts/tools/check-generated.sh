#!/bin/sh
# Regenerate projections and fail when tracked output or repository cleanliness drifts.
set -eu

package_dir=$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd -P)
cd "$package_dir"

"$package_dir/tools/generate.sh"

if ! git diff --exit-code -- gen; then
	printf '%s\n' 'OURO-CONTRACT-GENERATED-DRIFT: run tools/generate.sh and review the output.' >&2
	exit 1
fi

untracked=$(git ls-files --others --exclude-standard -- gen)
if [ -n "$untracked" ]; then
	printf '%s\n' 'OURO-CONTRACT-GENERATED-UNTRACKED: generation produced untracked output.' >&2
	printf '%s\n' "$untracked" >&2
	exit 1
fi
