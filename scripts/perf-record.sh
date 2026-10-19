#!/usr/bin/env bash
# Records the separated performance cells: process startup, local send cycle,
# contention, peak RSS and residual process count. No network, no credentials.
# Usage: scripts/perf-record.sh
set -euo pipefail

root=$(cd "$(dirname "$0")/.." && pwd)
cd "$root"
tmp=$(mktemp -d "${TMPDIR:-/tmp}/amgo-perf.XXXXXX")
trap 'rm -rf "$tmp"' EXIT
bin="$tmp/amgo"
home="$tmp/home"
mkdir -p "$home"

echo "== build"
CGO_ENABLED=0 go build -trimpath -o "$bin" ./cmd/amgo

echo "== process startup (50 runs)"
go test ./benchmark -run TestStartupDistribution -v

echo "== send cycle (prepare + dispatched marker + finalize)"
go test ./internal/store -run '^$' -bench 'BenchmarkPrepareSend|BenchmarkSendCycle' -benchmem

echo "== 16-process contention"
AMGO_CONTENTION=1 go test ./internal/store -run TestContentionMultiProcess -count=1

echo "== peak RSS"
rss_cmd() {
	local label="$1"
	shift
	if [ "$(uname -s)" = "Darwin" ]; then
		local out
		out=$(/usr/bin/time -l "$@" 2>&1)
		printf '%-14s %s bytes\n' "$label" "$(awk '/maximum resident set size/ {print $1}' <<<"$out")"
	else
		local out
		out=$(/usr/bin/time -v "$@" 2>&1)
		printf '%-14s %s KiB\n' "$label" "$(awk -F': ' '/Maximum resident set size/ {print $2}' <<<"$out")"
	fi
}
rss_cmd version "$bin" --home "$home" version
rss_cmd "doctor" "$bin" --home "$home" doctor --state
rss_cmd sessions "$bin" --home "$home" sessions

echo "== residual amgo processes"
if pgrep -f "$bin" >/dev/null 2>&1; then
	echo "FAIL: an amgo process survived:"
	pgrep -af "$bin"
	exit 1
fi
echo "0"
