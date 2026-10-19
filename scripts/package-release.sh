#!/usr/bin/env bash
# Packs each release binary in dist/ into amgo-<os>-<arch>.tar.gz with the LICENSE and
# README (REL-03), then rewrites dist/SHA256SUMS. Run it after scripts/sign-notarize.sh:
# tar keeps the signed darwin binaries byte for byte, and the archive keeps the
# executable bit, which a bare-binary download loses.
#
# The archives are a function of the files' contents alone, so a re-run of
# publish-release.sh reproduces the assets of a release it already created and can verify
# them byte for byte: fixed modes (not the umask's), a fixed UTC mtime (not the local time
# zone's), a fixed root owner (not the local account), no extended attributes or other
# platform metadata (ustar cannot carry them), and gzip -n (no name or time stamp).
set -euo pipefail

root="$(cd "$(dirname "$0")/.." && pwd)"
dist="${DIST:-${root}/dist}"
cd "$root"

# bsdtar (macOS) and GNU tar spell the same normalisations differently; any other tar
# cannot be told to drop them, so it is refused rather than trusted.
tar_version="$(tar --version 2>&1 | head -n 1)" || true
case "$tar_version" in
bsdtar*)
	tar_flags=(--format ustar --uid 0 --gid 0 --uname root --gname root
		--no-xattrs --no-acls --no-fflags --no-mac-metadata)
	;;
*"GNU tar"*)
	tar_flags=(--format=ustar --owner=root:0 --group=root:0
		--no-xattrs --no-acls --no-selinux)
	;;
*)
	echo "package-release.sh: tar is neither bsdtar nor GNU tar (${tar_version:-no version}); cannot normalise the archive" >&2
	exit 1
	;;
esac

staging="$(mktemp -d)"
trap 'rm -rf -- "$staging"' EXIT

for platform in darwin-arm64 darwin-amd64 linux-arm64 linux-amd64; do
	bin="${dist}/amgo-${platform}"
	if [ ! -x "$bin" ]; then
		echo "package-release.sh: ${bin} is missing; run make release first" >&2
		exit 1
	fi
	stage="${staging}/${platform}"
	mkdir -- "$stage"
	cp -- "$bin" "${stage}/amgo"
	cp -- LICENSE README.md "${stage}/"
	# No "--": macOS chmod rejects it, and these paths are absolute.
	chmod 0755 "${stage}/amgo"
	chmod 0644 "${stage}/LICENSE" "${stage}/README.md"
	TZ=UTC touch -t 200001010000 -- "${stage}/amgo" "${stage}/LICENSE" "${stage}/README.md"
	# COPYFILE_DISABLE keeps macOS tar from adding ._ AppleDouble files.
	COPYFILE_DISABLE=1 tar "${tar_flags[@]}" -cf - -C "$stage" amgo LICENSE README.md |
		gzip -n -9 >"${dist}/amgo-${platform}.tar.gz"
	echo "packed ${dist}/amgo-${platform}.tar.gz"
done

"${root}/scripts/checksums.sh" "$dist"
