#!/usr/bin/env bash
# The release checklist. Run after `make release` and scripts/sign-notarize.sh,
# before pushing the tag and publishing:
#
#   AMGO_RELEASE=1 scripts/release-check.sh
#
# It verifies the four release binaries' provenance, runs the native delivery
# gates against the checked-out commit, and writes a report that
# publish-release.sh requires.
#
#   AMGO_SKIP_NATIVE=1  skip the native gate suite (report says SKIPPED; a
#                       release is refused without a PASS)
#   DIST                build directory (default: dist)
set -euo pipefail

if [ "${AMGO_RELEASE:-}" != "1" ]; then
	echo "release-check.sh: set AMGO_RELEASE=1; the native gates launch live sessions and spend tokens" >&2
	exit 2
fi

root="$(cd "$(dirname "$0")/.." && pwd)"
cd "$root"
dist="${DIST:-dist}"
platforms=(darwin-arm64 darwin-amd64 linux-amd64 linux-arm64)

sha_of() {
	shasum -a 256 -- "$1" | awk '{print $1}'
}

head="$(git rev-parse HEAD)"
tag="$(git describe --tags --exact-match HEAD 2>/dev/null)" || {
	echo "release-check.sh: HEAD is not tagged; create the release tag first" >&2
	exit 1
}
[[ "$tag" =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]] || {
	echo "release-check.sh: ${tag} is not vX.Y.Z" >&2
	exit 1
}
version="${tag#v}"

status="$(git status --porcelain)" || {
	echo "release-check.sh: git status failed" >&2
	exit 1
}
[ -z "$status" ] || {
	echo "release-check.sh: the working tree is not clean; build from the tagged commit" >&2
	exit 1
}

for platform in "${platforms[@]}"; do
	bin="${dist}/amgo-${platform}"
	[ -s "$bin" ] || {
		echo "release-check.sh: ${bin} is missing; run make release" >&2
		exit 1
	}
	info="$(go version -m "$bin" 2>&1)" || {
		echo "release-check.sh: cannot read provenance from ${bin}: ${info}" >&2
		exit 1
	}
	rev="$(sed -n 's/^[[:space:]]*build[[:space:]]*vcs\.revision=//p' <<<"$info")"
	modified="$(sed -n 's/^[[:space:]]*build[[:space:]]*vcs\.modified=//p' <<<"$info")"
	[ "$rev" = "$head" ] || {
		echo "release-check.sh: ${bin} was built from ${rev:-an unknown commit}, not HEAD ${head}" >&2
		exit 1
	}
	[ "$modified" = false ] || {
		echo "release-check.sh: ${bin} was built from a modified tree (vcs.modified=${modified:-missing})" >&2
		exit 1
	}
done

native_platform="$(go env GOOS)-$(go env GOARCH)"
native_bin="${dist}/amgo-${native_platform}"
[ -x "$native_bin" ] || {
	echo "release-check.sh: ${native_bin} is not executable" >&2
	exit 1
}
out="$("$native_bin" version)" || {
	echo "release-check.sh: ${native_bin} version failed" >&2
	exit 1
}
[ "$out" = "amgo ${version}" ] || {
	echo "release-check.sh: version output is '${out}', want 'amgo ${version}'" >&2
	exit 1
}
checked_sha="$(sha_of "$native_bin")"

vuln_result="PASS"
echo "== govulncheck"
if ! go run golang.org/x/vuln/cmd/govulncheck@v1.9.0 ./...; then
	vuln_result="FAIL"
fi

native_result="SKIPPED"
if [ "${AMGO_SKIP_NATIVE:-}" != "1" ]; then
	native_log="${dist}/native-gates-$(date -u +%Y%m%dT%H%M%SZ).log"
	echo "== native delivery gates"
	if AMGO_NATIVE=1 go test -tags native -count=1 -timeout 20m ./tests/native -v 2>&1 | tee "$native_log"; then
		native_result="PASS"
	else
		native_result="FAIL"
	fi
fi

stamp="$(date -u +%Y%m%dT%H%M%SZ)"
report="${dist}/release-check-${stamp}.md"
{
	echo "# amgo release check"
	echo
	echo "- Version: amgo ${tag}"
	echo "- Commit: ${head}"
	echo "- Binary sha256: ${checked_sha}"
	for platform in "${platforms[@]}"; do
		bin="${dist}/amgo-${platform}"
		echo "- Release binary amgo-${platform} sha256: $(sha_of "$bin")"
		echo "- Release binary amgo-${platform} commit: ${head}"
	done
	echo "- Vulnerabilities: ${vuln_result}"
	echo "- Native gates: ${native_result}"
	echo
	if [ "$native_result" = "PASS" ] && [ "$vuln_result" = "PASS" ]; then
		echo "Result: PASS"
	else
		echo "Result: FAIL"
	fi
} >"$report"
cat "$report"
[ "$native_result" = "PASS" ] && [ "$vuln_result" = "PASS" ] || exit 1
