#!/usr/bin/env bash
# Fails when a module imported by amgo or its tests is missing from
# THIRD-PARTY-NOTICES.md, has no license file in the module cache, or when a
# notice row is no longer used by any package. The scope is code compiled into
# a release binary plus code imported by tests; modules that exist only in the
# module graph are not distributed and are not inventoried.
set -euo pipefail

root="$(cd "$(dirname "$0")/.." && pwd)"
cd "$root"
notices="${1:-THIRD-PARTY-NOTICES.md}"
tmp="$(mktemp -d "${TMPDIR:-/tmp}/amgo-license.XXXXXX")"
trap 'rm -rf -- "$tmp"' EXIT
used="$tmp/used"
listed="$tmp/listed"

go list -deps -test -tags native -f '{{if .Module}}{{if not .Module.Main}}{{.Module.Path}} {{.Module.Version}}{{end}}{{end}}' ./... |
	grep -v '^$' | sort -u >"$used"

awk '
	/<!-- BEGIN dependency table -->/ { in_table = 1; next }
	/<!-- END dependency table -->/ { in_table = 0; next }
	in_table && /^\|/ {
		n = split($0, f, "|")
		if (n < 4) next
		mod = f[2]
		ver = f[3]
		gsub(/^[ \t]+|[ \t]+$/, "", mod)
		gsub(/^[ \t]+|[ \t]+$/, "", ver)
		if (mod == "Module" || mod ~ /^-+$/) next
		print mod " " ver
	}
' "$notices" | sort -u >"$listed"

fail=0
while read -r mod ver; do
	[ -n "$mod" ] || continue
	if ! grep -Fq "| ${mod} | ${ver} |" "$notices"; then
		echo "license-check: ${mod} ${ver} is used but missing from ${notices}" >&2
		fail=1
	fi
	if ! dir="$(go list -m -f '{{.Dir}}' "$mod")"; then
		echo "license-check: cannot resolve the module directory for ${mod} ${ver}" >&2
		fail=1
		continue
	fi
	if [ -z "$dir" ] || [ ! -d "$dir" ]; then
		echo "license-check: no module directory for ${mod} ${ver}" >&2
		fail=1
		continue
	fi
	lic="$(find "$dir" -maxdepth 1 -type f \( -name 'LICENSE*' -o -name 'COPYING*' -o -name 'NOTICE*' \) -print -quit)"
	if [ -z "$lic" ]; then
		echo "license-check: no license file in ${dir}" >&2
		fail=1
	fi
done <"$used"

while read -r mod ver; do
	[ -n "$mod" ] || continue
	if ! grep -Fqx "${mod} ${ver}" "$used"; then
		echo "license-check: ${mod} ${ver} is listed but no longer used; remove its row and license text" >&2
		fail=1
	fi
done <"$listed"

if [ "$fail" -ne 0 ]; then
	echo "license-check: FAIL" >&2
	exit 1
fi
printf 'license-check: PASS (%s modules)\n' "$(wc -l <"$used" | tr -d ' ')"
