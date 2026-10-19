#!/usr/bin/env bash
# Writes DIR/SHA256SUMS for every amgo-* binary in DIR (REL-01). Re-run after
# signing: codesign rewrites the darwin binaries, so their old sums are stale.
set -euo pipefail

dir="${1:?usage: checksums.sh DIR}"
cd "$dir"
shopt -s nullglob
bins=(amgo-*-*)
if [ "${#bins[@]}" -eq 0 ]; then
	echo "checksums.sh: no amgo-* binaries in $dir" >&2
	exit 1
fi
if command -v sha256sum >/dev/null 2>&1; then
	sha256sum -- "${bins[@]}" >SHA256SUMS.tmp
else
	shasum -a 256 -- "${bins[@]}" >SHA256SUMS.tmp
fi
mv SHA256SUMS.tmp SHA256SUMS
cat SHA256SUMS
