#!/usr/bin/env bash
# Refuses a tap update that would not move the formula forward (REL-03: upgrading never
# downgrades). Exit 0 when NEW is a canonical X.Y.Z release (no leading zeros, no
# prerelease, no build metadata: the tap publishes stable releases only) whose SemVer 2.0
# precedence is strictly higher than CURRENT's, or CURRENT is empty (no formula yet); exit 1
# otherwise: equal, older, or not a valid version. A leading "v" on either argument is
# dropped.
#
# CURRENT may be any valid SemVer 2.0 version, prerelease included (X.Y.Z-rc.1 < X.Y.Z);
# its build metadata takes no part in precedence. A CURRENT that is not valid SemVer is
# never assumed older: its order is not guessed, it is refused.
#
#   scripts/tap-version-guard.sh CURRENT NEW
#
# Sourcing the script defines semver_cmp without running the guard.

# Alphanumeric identifiers compare in ASCII order, not the locale's collation.
LC_ALL=C

# SemVer 2.0's grammar. A numeric identifier is 0 or digits without a leading zero; an
# alphanumeric one holds at least one letter or hyphen; build identifiers may be any
# nonempty run of [0-9A-Za-z-].
semver_num='(0|[1-9][0-9]*)'
semver_pre_id='(0|[1-9][0-9]*|[0-9A-Za-z-]*[A-Za-z-][0-9A-Za-z-]*)'
semver_build_id='[0-9A-Za-z-]+'
semver_release="^${semver_num}\.${semver_num}\.${semver_num}\$"
semver_full="^${semver_num}\.${semver_num}\.${semver_num}(-${semver_pre_id}(\.${semver_pre_id})*)?(\+${semver_build_id}(\.${semver_build_id})*)?\$"

# cmp_num A B prints -1, 0 or 1 for two canonical numbers. Without leading zeros the
# longer one is larger and equal lengths order as strings, so no number of any length is
# ever parsed as an integer, and none can overflow.
cmp_num() {
	if [ "${#1}" -lt "${#2}" ]; then
		echo -1
	elif [ "${#1}" -gt "${#2}" ]; then
		echo 1
	elif [ "$1" = "$2" ]; then
		echo 0
	elif [[ "$1" < "$2" ]]; then
		echo -1
	else
		echo 1
	fi
}

# cmp_pre_id A B prints -1, 0 or 1 for two prerelease identifiers: numeric ones compare
# numerically, alphanumeric ones in ASCII order, and a numeric one is lower than an
# alphanumeric one.
cmp_pre_id() {
	local an=0 bn=0
	[[ "$1" =~ ^[0-9]+$ ]] && an=1
	[[ "$2" =~ ^[0-9]+$ ]] && bn=1
	if [ "$an" = 1 ] && [ "$bn" = 1 ]; then
		cmp_num "$1" "$2"
	elif [ "$an" = 1 ]; then
		echo -1
	elif [ "$bn" = 1 ]; then
		echo 1
	elif [ "$1" = "$2" ]; then
		echo 0
	elif [[ "$1" < "$2" ]]; then
		echo -1
	else
		echo 1
	fi
}

# semver_cmp A B prints -1, 0 or 1: the SemVer 2.0 precedence of A against B, two versions
# that match semver_full. Build metadata is dropped first; then the core compares field by
# field, a version without a prerelease is higher than one with, and prereleases compare
# identifier by identifier, a shorter set lower when every identifier it has is equal.
semver_cmp() {
	local a="${1%%+*}" b="${2%%+*}" a_core a_pre="" b_core b_pre="" order i
	a_core="${a%%-*}"
	b_core="${b%%-*}"
	[ "$a_core" = "$a" ] || a_pre="${a#*-}"
	[ "$b_core" = "$b" ] || b_pre="${b#*-}"
	local a1 a2 a3 b1 b2 b3
	IFS=. read -r a1 a2 a3 <<<"$a_core"
	IFS=. read -r b1 b2 b3 <<<"$b_core"
	for i in 1 2 3; do
		case "$i" in
		1) order="$(cmp_num "$a1" "$b1")" ;;
		2) order="$(cmp_num "$a2" "$b2")" ;;
		3) order="$(cmp_num "$a3" "$b3")" ;;
		esac
		if [ "$order" != 0 ]; then
			echo "$order"
			return
		fi
	done
	if [ -z "$a_pre" ] && [ -z "$b_pre" ]; then
		echo 0
		return
	elif [ -z "$a_pre" ]; then
		echo 1
		return
	elif [ -z "$b_pre" ]; then
		echo -1
		return
	fi
	local -a ap bp
	IFS=. read -r -a ap <<<"$a_pre"
	IFS=. read -r -a bp <<<"$b_pre"
	i=0
	while [ "$i" -lt "${#ap[@]}" ] && [ "$i" -lt "${#bp[@]}" ]; do
		order="$(cmp_pre_id "${ap[$i]}" "${bp[$i]}")"
		if [ "$order" != 0 ]; then
			echo "$order"
			return
		fi
		i=$((i + 1))
	done
	cmp_num "${#ap[@]}" "${#bp[@]}"
}

tap_version_guard() {
	if [ "$#" -ne 2 ]; then
		echo "usage: tap-version-guard.sh CURRENT NEW" >&2
		return 2
	fi
	local current="${1#v}" new="${2#v}"
	if ! [[ "$new" =~ $semver_release ]]; then
		echo "tap-version-guard.sh: ${new} is not a canonical X.Y.Z release version" >&2
		return 1
	fi
	if [ -z "$current" ]; then
		return 0
	fi
	if ! [[ "$current" =~ $semver_full ]]; then
		echo "tap-version-guard.sh: the tap's ${current} is not a valid SemVer version" >&2
		return 1
	fi
	if [ "$current" = "$new" ]; then
		echo "tap-version-guard.sh: the tap already has ${new}" >&2
		return 1
	fi
	if [ "$(semver_cmp "$new" "$current")" != 1 ]; then
		echo "tap-version-guard.sh: ${new} is not newer than the tap's ${current}" >&2
		return 1
	fi
}

if [ "${BASH_SOURCE[0]}" = "$0" ]; then
	set -euo pipefail
	tap_version_guard "$@"
fi
