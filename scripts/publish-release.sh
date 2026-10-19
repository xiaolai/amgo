#!/usr/bin/env bash
# Publishes a release that was built, signed and checked locally:
#
#   git tag -a vX.Y.Z -m "amgo X.Y.Z"       # local only until the check passes
#   make release                             # binaries carry the tag as their version
#   AMGO_SIGN_IDENTITY=... AMGO_NOTARY_PROFILE=... scripts/sign-notarize.sh
#   AMGO_RELEASE=1 scripts/release-check.sh  # must end "Result: PASS"
#   git push origin vX.Y.Z
#   scripts/publish-release.sh vX.Y.Z
#
# Before any network write it refuses unless the tag exists locally and on
# xiaolai/amgo and both point at HEAD; the tree is clean; dist/ holds a passing
# release-check report for exactly this version whose newest run checked a build
# of HEAD and records matching sha256 values for every release binary and its
# native binary; and both darwin binaries carry a Developer ID signature with
# the hardened runtime and an Accepted notarisation of their current bytes.
#
# It then packages the archives, creates the GitHub release, checks every asset
# downloads anonymously and matches, updates the Homebrew tap (never backwards),
# and dispatches the clean-Mac Gatekeeper check. Re-running after a later step
# failed is safe: an existing release must hold identical assets, and a tap that
# already holds this exact formula is left alone.
set -euo pipefail

repo="xiaolai/amgo"
tap="xiaolai/homebrew-tap"
root="$(cd "$(dirname "$0")/.." && pwd)"
dist="${DIST:-${root}/dist}"

fail() {
	echo "publish-release.sh: $*" >&2
	exit 1
}
sha_of() {
	shasum -a 256 -- "$1" | awk '{print $1}'
}

if [ "$#" -ne 1 ]; then
	echo "usage: publish-release.sh vX.Y.Z" >&2
	exit 2
fi
version="$1"
[[ "$version" =~ ^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$ ]] || fail "${version} is not a vX.Y.Z version"
bare="${version#v}"
cd "$root"

# --- Local checks; nothing below this block writes to the network.

head="$(git rev-parse HEAD)"
local_tag="$(git rev-parse --verify --quiet "refs/tags/${version}^{commit}")" ||
	fail "tag ${version} does not exist locally"
[ "$local_tag" = "$head" ] || fail "tag ${version} is ${local_tag}, not HEAD ${head}"

read -r tag_type tag_sha < <(gh api "repos/${repo}/git/ref/tags/${version}" --jq '.object.type + " " + .object.sha') ||
	fail "tag ${version} is not on ${repo}; push it first"
while [ "$tag_type" = tag ]; do
	read -r tag_type tag_sha < <(gh api "repos/${repo}/git/tags/${tag_sha}" --jq '.object.type + " " + .object.sha') ||
		fail "cannot read tag object ${tag_sha} on ${repo}"
done
[ "$tag_type" = commit ] || fail "${repo}'s tag ${version} names a ${tag_type:-nothing}, not a commit"
[ "$tag_sha" = "$head" ] || fail "${repo}'s tag ${version} is ${tag_sha}, not HEAD ${head}"

tree_status="$(git status --porcelain)" || fail "git status failed; cannot establish that the working tree is clean"
[ -z "$tree_status" ] || fail "the working tree is not clean"

report=""
shopt -s nullglob
for r in "${dist}"/release-check-*.md; do
	if grep -Fxq -- "- Version: amgo ${version}" "$r"; then
		report="$r"
	fi
done
shopt -u nullglob
[ -n "$report" ] || fail "no release-check report for ${version} in ${dist}; run AMGO_RELEASE=1 scripts/release-check.sh"
grep -Eq '^Result: PASS( |$)' "$report" || fail "${report} did not pass"
grep -Fxq -- "- Commit: ${head}" "$report" || fail "${report} did not check HEAD ${head}"

platforms=(darwin-arm64 darwin-amd64 linux-amd64 linux-arm64)
checked_shas=()
for platform in "${platforms[@]}"; do
	rb="${dist}/amgo-${platform}"
	recorded="$(sed -n "s/^- Release binary amgo-${platform} sha256: \([0-9a-f]\{64\}\)\$/\1/p" "$report")"
	[ -n "$recorded" ] || fail "${report} records no sha256 for amgo-${platform}"
	[ -s "$rb" ] || fail "${rb} is missing; run make release and scripts/sign-notarize.sh"
	[ "$(sha_of "$rb")" = "$recorded" ] ||
		fail "amgo-${platform} is not the binary ${report} checked"
	built="$(sed -n "s/^- Release binary amgo-${platform} commit: \([0-9a-f]\{40\}\)\$/\1/p" "$report")"
	[ "$built" = "$head" ] || fail "amgo-${platform} was built from ${built:-an unknown commit}, not HEAD"
	checked_shas+=("$recorded")
done
checked_sha="$(sed -n 's/^- Binary sha256: \([0-9a-f]\{64\}\)$/\1/p' "$report")"
[ -n "$checked_sha" ] || fail "${report} records no binary sha256"
checked=""
for platform in "${platforms[@]}"; do
	if [ -s "${dist}/amgo-${platform}" ] && [ "$(sha_of "${dist}/amgo-${platform}")" = "$checked_sha" ]; then
		checked="$platform"
	fi
done
[ -n "$checked" ] || fail "${report} checked a binary that is not one of the release binaries"

for arch in arm64 amd64; do
	bin="${dist}/amgo-darwin-${arch}"
	[ -s "$bin" ] || fail "${bin} is missing"
	codesign --verify --strict --verbose=2 "$bin"
	sig="$(codesign -dvvv "$bin" 2>&1)"
	grep -Eq 'flags=0x[0-9a-f]+\(runtime\)' <<<"$sig" || fail "${bin} lacks the hardened runtime flag"
	grep -Eq '^Authority=Developer ID Application: ' <<<"$sig" || fail "${bin} is not signed with a Developer ID Application identity"
	info="${dist}/notary-info-darwin-${arch}.txt"
	[ -s "$info" ] || fail "${info} is missing; run scripts/sign-notarize.sh"
	grep -Eq '^ *status: Accepted$' "$info" || fail "${info} does not say Accepted"
	notarised="${dist}/notary-sha256-darwin-${arch}.txt"
	[ -s "$notarised" ] || fail "${notarised} is missing"
	[ "$(cat -- "$notarised")" = "$(sha_of "$bin")" ] || fail "${bin} is not the binary that was notarised"
done

# --- Package and publish.

DIST="$dist" "${root}/scripts/package-release.sh"
stage="$(mktemp -d)"
trap 'rm -rf -- "$stage"' EXIT
assets=()
for platform in "${platforms[@]}"; do
	assets+=("amgo-${platform}.tar.gz")
done

check_archives() {
	local i a members inner
	for i in "${!platforms[@]}"; do
		a="amgo-${platforms[$i]}.tar.gz"
		members="$(tar -tzf "${1}/${a}" | LC_ALL=C sort | tr '\n' ' ')" ||
			fail "${a} is not a readable gzip tar archive"
		[ "$members" = "LICENSE README.md amgo " ] ||
			fail "${a} holds ${members:-nothing}, not exactly amgo, LICENSE and README.md"
		inner="$(tar -xzOf "${1}/${a}" amgo | shasum -a 256 | awk '{print $1}')" ||
			fail "cannot read amgo from ${a}"
		[ "$inner" = "${checked_shas[$i]}" ] ||
			fail "${a} holds an amgo that is not the checked amgo-${platforms[$i]}"
	done
}

for a in "${assets[@]}"; do
	cp -p -- "${dist}/${a}" "${stage}/${a}"
done
check_archives "$stage"
(cd "$stage" && shasum -a 256 -- "${assets[@]}" >SHA256SUMS)
cp -- "$report" "${stage}/release-check-${version}.md"

notes="${stage}/notes.md"
cat >"$notes" <<NOTES
Install with \`brew install xiaolai/tap/amgo\`, or download the archive for your platform,
check it against \`SHA256SUMS\` and put \`amgo\` on your \`PATH\`.

The macOS binaries are signed with a Developer ID and notarised; the first run needs a
network connection so Gatekeeper can fetch the notarisation ticket.
\`release-check-${version}.md\` records the native gates and binary hashes this build passed.
NOTES

if gh release view "$version" --repo "$repo" >/dev/null 2>&1; then
	echo "release ${version} already exists; checking its assets match this build"
else
	gh release create "$version" --repo "$repo" --verify-tag --title "amgo ${version}" \
		--notes-file "$notes" \
		"${stage}/${assets[0]}" "${stage}/${assets[1]}" "${stage}/${assets[2]}" "${stage}/${assets[3]}" \
		"${stage}/SHA256SUMS" "${stage}/release-check-${version}.md"
fi

download="${stage}/download"
mkdir -p -- "$download"
for a in "${assets[@]}" SHA256SUMS "release-check-${version}.md"; do
	curl --fail --silent --show-error --location --retry 5 --retry-all-errors \
		-o "${download}/${a}" "https://github.com/${repo}/releases/download/${version}/${a}" ||
		fail "${a} does not download from release ${version}"
	cmp -- "${stage}/${a}" "${download}/${a}" ||
		fail "release ${version}'s ${a} differs from this build's"
done
[ "$(cd "$download" && shasum -a 256 -- "${assets[@]}")" = "$(cat -- "${download}/SHA256SUMS")" ] ||
	fail "release ${version}'s SHA256SUMS does not list exactly the archives"
check_archives "$download"
echo "all release assets download anonymously and match"

# --- Homebrew tap.

DIST="$dist" "${root}/scripts/homebrew-formula.sh" "$version" \
	"https://github.com/${repo}/releases/download" "https://github.com/${repo}"
tapdir="${stage}/tap"
gh repo clone "$tap" "$tapdir" -- --depth 1 --branch main
formula="${tapdir}/Formula/amgo.rb"
current=""
if [ -f "$formula" ]; then
	current="$(sed -n 's/^ *VERSION = "\([^"]*\)".*$/\1/p' "$formula")"
	[ -n "$current" ] || fail "the tap's Formula/amgo.rb has no readable VERSION; refusing to replace it"
fi
if [ "$current" = "$bare" ] && cmp -s -- "${dist}/amgo.rb" "$formula"; then
	echo "tap already has ${bare}"
else
	"${root}/scripts/tap-version-guard.sh" "$current" "$bare" ||
		fail "the tap has ${current:-no version}; refusing to replace it with ${bare}"
	mkdir -p -- "${tapdir}/Formula"
	cp -- "${dist}/amgo.rb" "$formula"
	brew style "$formula"
	msg="${stage}/commit-message.txt"
	printf 'amgo %s\n' "$bare" >"$msg"
	git -C "$tapdir" add Formula/amgo.rb
	git -C "$tapdir" commit -F "$msg"
	git -C "$tapdir" push origin HEAD:main
	echo "tap updated to ${bare}"
fi

gh workflow run clean-mac.yml --repo "$repo" --ref main -f version="$version"
echo "dispatched clean-mac.yml for ${version}; follow it with: gh run list --repo ${repo} --workflow clean-mac.yml"
