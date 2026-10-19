#!/usr/bin/env bash
# Signs and notarises the darwin binaries in dist/. Run after `make release` and
# before scripts/release-check.sh.
#
# The login keychain must hold a Developer ID Application identity and a
# notarytool profile. The profile can be created from the local .env once:
#
#   set -a; . ./.env; set +a
#   xcrun notarytool store-credentials amgo-notary \
#     --key "$HOME/.appstoreconnect/private_keys/AuthKey_${ASC_KEY_ID}.p8" \
#     --key-id "$ASC_KEY_ID" --issuer "$ASC_ISSUER_ID"
#
#   AMGO_SIGN_IDENTITY="Developer ID Application: NAME (TEAMID)" \
#     AMGO_NOTARY_PROFILE=amgo-notary scripts/sign-notarize.sh
#
# A bare Mach-O binary cannot be stapled, so the notarisation ticket stays on
# Apple's servers and Gatekeeper fetches it online on first run.
set -euo pipefail

identity="${AMGO_SIGN_IDENTITY:?set AMGO_SIGN_IDENTITY to the Developer ID Application identity}"
profile="${AMGO_NOTARY_PROFILE:?set AMGO_NOTARY_PROFILE to the notarytool keychain profile}"
dist="${DIST:-dist}"
here="$(cd "$(dirname "$0")" && pwd)"
stage="$(mktemp -d)"
trap 'rm -rf -- "$stage"' EXIT

sha_of() {
	shasum -a 256 -- "$1" | awk '{print $1}'
}

for arch in arm64 amd64; do
	bin="${dist}/amgo-darwin-${arch}"
	if [ ! -s "$bin" ]; then
		echo "sign-notarize.sh: ${bin} is missing; run make release first" >&2
		exit 1
	fi

	echo "== sign ${bin}"
	codesign --force --sign "$identity" --options runtime --timestamp "$bin"
	codesign --verify --strict --verbose=2 "$bin"
	sig="$(codesign --display --verbose=2 "$bin" 2>&1)"
	if ! grep -Eq 'flags=0x[0-9a-f]+\(runtime\)' <<<"$sig"; then
		echo "sign-notarize.sh: ${bin} lacks the hardened runtime flag" >&2
		exit 1
	fi

	# Apple notarises the bytes uploaded, which must be the bytes recorded. A
	# read-only copy is zipped and hashed before upload.
	snapshot="${stage}/amgo-darwin-${arch}"
	cp -- "$bin" "$snapshot"
	chmod a-w "$snapshot"
	submitted_sha="$(sha_of "$snapshot")"
	zip="${dist}/amgo-darwin-${arch}.zip"
	rm -f -- "$zip"
	ditto -c -k --keepParent "$snapshot" "$zip"

	echo "== notarise ${zip}"
	submit_log="${dist}/notary-submit-darwin-${arch}.txt"
	# `submit --wait` exits 0 even for an Invalid submission, so the info status decides.
	xcrun notarytool submit "$zip" --wait --keychain-profile "$profile" | tee "$submit_log"
	id="$(awk '$1 == "id:" {print $2; exit}' "$submit_log")"
	if [ -z "$id" ]; then
		echo "sign-notarize.sh: no submission id in ${submit_log}" >&2
		exit 1
	fi
	info_log="${dist}/notary-info-darwin-${arch}.txt"
	xcrun notarytool info "$id" --keychain-profile "$profile" | tee "$info_log"
	if ! grep -Eq '^ *status: Accepted$' "$info_log"; then
		echo "sign-notarize.sh: submission ${id} is not Accepted; inspect it with: xcrun notarytool log ${id} --keychain-profile ${profile}" >&2
		exit 1
	fi
	if [ "$(sha_of "$bin")" != "$submitted_sha" ]; then
		echo "sign-notarize.sh: ${bin} changed while it was notarised (submitted sha256 ${submitted_sha}); run the script again" >&2
		exit 1
	fi
	echo "$submitted_sha" >"${dist}/notary-sha256-darwin-${arch}.txt"
done

"${here}/checksums.sh" "$dist"
