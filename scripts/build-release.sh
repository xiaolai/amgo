#!/usr/bin/env bash
# Cross-compiles amgo for the four release platforms into dist/.
# VERSION is injected into internal/cli.Version, so `amgo version` reports it.
# Called by `make release`.
set -euo pipefail

: "${VERSION:?set VERSION}"
dist="${DIST:-dist}"
version="${VERSION#v}"
ldflags="-s -w -X amgo/internal/cli.Version=${version}"

rm -rf -- "$dist"
mkdir -p -- "$dist"
for target in darwin/arm64 darwin/amd64 linux/amd64 linux/arm64; do
	os="${target%/*}"
	arch="${target#*/}"
	out="${dist}/amgo-${os}-${arch}"
	echo "build ${out}"
	CGO_ENABLED=0 GOOS="$os" GOARCH="$arch" \
		go build -trimpath -ldflags "$ldflags" -o "$out" ./cmd/amgo
done
"$(dirname "$0")/checksums.sh" "$dist"
