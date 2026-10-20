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
vuln_source="vuln.go.dev"
vuln_mirror_date=""
echo "== govulncheck"
vuln_log="${dist}/govulncheck-$(date -u +%Y%m%dT%H%M%SZ).log"
run_govuln() {
	go run golang.org/x/vuln/cmd/govulncheck@v1.9.0 "$@"
}
if ! run_govuln ./... >"$vuln_log" 2>&1; then
	# A blocked vuln.go.dev is a transport failure, not a vulnerability
	# finding. Fall back to the Go team's public database bucket and record
	# the source and its build date in the report.
	if grep -q 'fetching vulnerabilities' "$vuln_log"; then
		vuln_source="storage.googleapis.com/go-vulndb (official mirror)"
		vuln_mirror_date="$(curl -sSI --max-time 15 \
			https://storage.googleapis.com/go-vulndb/index/modules.json.gz 2>/dev/null |
			sed -n 's/^last-modified: //Ip' | tr -d '\r')"
		if ! run_govuln -db https://storage.googleapis.com/go-vulndb ./... >>"$vuln_log" 2>&1; then
			vuln_result="FAIL"
		fi
	else
		vuln_result="FAIL"
	fi
fi
cat "$vuln_log"

license_result="PASS"
echo "== license inventory"
if ! ./scripts/license-check.sh; then
	license_result="FAIL"
fi

native_result="SKIPPED"
native_bin_note="not run"
native_json=""
case "${ANTHROPIC_BASE_URL:-}" in
	*[![:space:]]*) claude_backend="custom" ;;
	*) claude_backend="default" ;;
esac
if [ "${AMGO_SKIP_NATIVE:-}" != "1" ]; then
	stamp_native="$(date -u +%Y%m%dT%H%M%SZ)"
	native_json="${dist}/native-gates-${stamp_native}.json"
	native_log="${dist}/native-gates-${stamp_native}.log"
	required_manifest="tests/native/required.txt"
	listed="$(mktemp "${TMPDIR:-/tmp}/amgo-native-listed.XXXXXX")"
	required_sorted="$(mktemp "${TMPDIR:-/tmp}/amgo-native-required.XXXXXX")"
	trap 'rm -f -- "$listed" "$required_sorted"' EXIT
	echo "== native delivery gates"
	[ -s "$required_manifest" ] || {
		echo "release-check.sh: ${required_manifest} is missing or empty" >&2
		native_result="FAIL"
	}
	AMGO_NATIVE=1 AMGO_NATIVE_BIN="$native_bin" \
		go test -tags native -list '^Test' ./tests/native 2>>"$native_log" |
		grep '^Test' | sort >"$listed" || true
	grep -E '^Test' "$required_manifest" | sort >"$required_sorted" || true
	if ! diff -u "$required_sorted" "$listed" >>"$native_log" 2>&1; then
		echo "release-check.sh: native test set differs from ${required_manifest}; update the manifest deliberately" >&2
		native_result="FAIL"
	fi
	if [ "$native_result" != "FAIL" ]; then
		# Run the gates against the signed release artifact whose hash is
		# recorded below, not a separately rebuilt binary.
		if AMGO_NATIVE=1 AMGO_NATIVE_BIN="$native_bin" \
			go test -tags native -count=1 -timeout 20m -json ./tests/native 2>>"$native_log" |
			tee "$native_json" >/dev/null; then
			if go run ./scripts/native-gate-check -required "$required_manifest" -json "$native_json" \
				2>>"$native_log"; then
				native_result="PASS"
			else
				native_result="FAIL"
			fi
		else
			native_result="FAIL"
		fi
	fi
	native_bin_note="${native_bin} sha256=${checked_sha}"
	if [ "$native_result" != "PASS" ]; then
		echo "release-check.sh: native gates did not produce a full PASS; see ${native_json}" >&2
		tail -20 "$native_log" >&2 || true
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
	echo "- Vulnerabilities: ${vuln_result} (${vuln_source}${vuln_mirror_date:+, last-modified ${vuln_mirror_date}})"
	echo "- License inventory: ${license_result}"
	echo "- Native gates: ${native_result}"
	echo "- Native gates binary: ${native_bin_note}"
	echo "- Native gates evidence: ${native_json:-none}"
	echo "- Claude backend: ${claude_backend}"
	echo
	if [ "$native_result" = "PASS" ] && [ "$vuln_result" = "PASS" ] && [ "$license_result" = "PASS" ]; then
		echo "Result: PASS"
	else
		echo "Result: FAIL"
	fi
} >"$report"
cat "$report"
[ "$native_result" = "PASS" ] && [ "$vuln_result" = "PASS" ] && [ "$license_result" = "PASS" ] || exit 1
