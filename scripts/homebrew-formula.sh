#!/usr/bin/env bash
# Renders the Homebrew formula (REL-03) from packaging/homebrew/amgo.rb.tmpl and the
# checksums in dist/SHA256SUMS. Run it after scripts/package-release.sh, so the sums are
# those of the archives that hold the signed binaries. It only writes dist/amgo.rb;
# publishing the formula to a tap is a separate step.
#
#   scripts/homebrew-formula.sh VERSION BASE_URL HOMEPAGE
#
# VERSION is the release version (a leading "v" is dropped), BASE_URL the releases
# download URL that v<VERSION>/amgo-<os>-<arch>.tar.gz is appended to (no trailing
# slash), HOMEPAGE the project page.
set -euo pipefail

if [ "$#" -ne 3 ]; then
	echo "usage: homebrew-formula.sh VERSION BASE_URL HOMEPAGE" >&2
	exit 2
fi
version="${1#v}"
base_url="$2"
homepage="$3"
dist="${DIST:-dist}"
root="$(cd "$(dirname "$0")/.." && pwd)"
tmpl="${root}/packaging/homebrew/amgo.rb.tmpl"
sums="${dist}/SHA256SUMS"
out="${dist}/amgo.rb"

# The values are substituted into Ruby string literals; reject anything that could
# break out of one.
if ! [[ "$version" =~ ^[0-9]+\.[0-9]+\.[0-9]+([.-][0-9A-Za-z.]+)?$ ]]; then
	echo "homebrew-formula.sh: version ${version} is not semver-like" >&2
	exit 2
fi
for u in "$base_url" "$homepage"; do
	if ! [[ "$u" =~ ^https://[A-Za-z0-9./_~%+-]+$ ]] || [[ "$u" == */ ]]; then
		echo "homebrew-formula.sh: ${u} is not an https URL without a trailing slash" >&2
		exit 2
	fi
done
if [ ! -s "$sums" ]; then
	echo "homebrew-formula.sh: ${sums} is missing; run make release first" >&2
	exit 1
fi

sum_of() {
	local s
	s="$(awk -v f="$1" '$2 == f || $2 == "*" f {print $1}' "$sums")"
	if ! [[ "$s" =~ ^[0-9a-f]{64}$ ]]; then
		echo "homebrew-formula.sh: no sha256 for $1 in ${sums}" >&2
		exit 1
	fi
	printf '%s' "$s"
}

formula="$(cat "$tmpl")"
formula="${formula//@VERSION@/$version}"
formula="${formula//@BASE_URL@/$base_url}"
formula="${formula//@HOMEPAGE@/$homepage}"
formula="${formula//@SHA256_DARWIN_ARM64@/$(sum_of amgo-darwin-arm64.tar.gz)}"
formula="${formula//@SHA256_DARWIN_AMD64@/$(sum_of amgo-darwin-amd64.tar.gz)}"
formula="${formula//@SHA256_LINUX_ARM64@/$(sum_of amgo-linux-arm64.tar.gz)}"
formula="${formula//@SHA256_LINUX_AMD64@/$(sum_of amgo-linux-amd64.tar.gz)}"
if grep -Eq '@[A-Z0-9_]+@' <<<"$formula"; then
	echo "homebrew-formula.sh: unfilled placeholder in the formula" >&2
	exit 1
fi
printf '%s\n' "$formula" >"$out"
echo "wrote ${out}"
