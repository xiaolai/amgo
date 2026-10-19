#!/usr/bin/env bash
# Scan the publication set, not the working directory.
#
# The publication set is every tracked file plus every untracked file that Git
# does not ignore. Ignored local material (.env, dev-docs, .cc-suite, editor
# state) is never part of a normal commit and is not scanned here; the
# forbidden-path check stops it if someone forces it into the index.
#
# gitleaks redacts matches. This script never prints file contents.
set -euo pipefail

root=$(git rev-parse --show-toplevel 2>/dev/null) || {
  echo "secret-scan: not inside a Git working tree" >&2
  exit 2
}
cd "$root"

if ! command -v gitleaks >/dev/null 2>&1; then
  echo "secret-scan: gitleaks is required (v8.30.1 or newer)" >&2
  exit 2
fi

work=$(mktemp -d "${TMPDIR:-/tmp}/amgo-secrets.XXXXXX")
trap 'rm -rf "$work"' EXIT
chmod 700 "$work"
tmp="$work/tree"
mkdir -p "$tmp"
config="$work/gitleaks.toml"
cat "$root/.gitleaks.toml" > "$config"
if [ -f "$root/.gitleaks.local.toml" ]; then
  printf '\n' >> "$config"
  cat "$root/.gitleaks.local.toml" >> "$config"
fi

count=0
bad=0
while IFS= read -r -d '' file; do
  case "$file" in
    .env.example|.env.sample|*/.env.example|*/.env.sample)
      ;;
    .env|.env.*|*/.env|*/.env.*|auth.json|*/auth.json|\
    credentials.json|*/credentials.json|*.key|*.pem|*.p12|*.pfx|\
    .netrc|*/.netrc|dev-docs/*|*/.cc-suite/*|.cc-suite/*|\
    .claude/*|*/.claude/*|.codex/*|*/.codex/*|.agents/*|*/.agents/*)
      echo "secret-scan: forbidden publication path: $file" >&2
      bad=1
      continue
      ;;
  esac
  mkdir -p "$tmp/$(dirname "$file")"
  cp -p "$file" "$tmp/$file"
  count=$((count + 1))
done < <(git ls-files -z --cached --others --exclude-standard)

if [ "$bad" -ne 0 ]; then
  echo "secret-scan: refusing to scan a set containing forbidden paths" >&2
  exit 1
fi

echo "secret-scan: publication set: $count files"
gitleaks dir "$tmp" \
  --config "$config" \
  --redact \
  --no-banner \
  -v

commits=$(git rev-list --all --count 2>/dev/null || echo 0)
if [ "$commits" -gt 0 ]; then
  echo "secret-scan: history: $commits commits"
  gitleaks git \
    --config "$config" \
    --redact \
    --no-banner \
    -v
else
  echo "secret-scan: history: no commits yet (nothing to scan)"
fi
