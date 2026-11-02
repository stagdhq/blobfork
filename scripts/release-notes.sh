#!/usr/bin/env bash
# A release's notes, from the commits since the previous tag: the tag's own
# message as the summary, then what's new (feat), fixes (fix) and the rest
# worth saying (docs, perf, refactor); chores are left out. The release
# workflow makes them the GitHub release's notes, which are the changelog.
#
#   scripts/release-notes.sh v0.1.0 > notes.md
#
# Commits say what they are the conventional way: "feat(s3): …", "fix: …";
# a "!" before the colon marks a breaking change.
set -euo pipefail

tag="$1"
repo="https://github.com/stagdhq/blobfork"
prev="$(git describe --tags --abbrev=0 "$tag^" 2>/dev/null || true)"
range="${prev:+$prev..}$tag"

# The annotated tag's message, without a leading "v1.2.3: ".
summary="$(git tag -l --format='%(contents)' "$tag" | sed -E "1s/^$tag:[[:space:]]*//")"

if [ -n "$summary" ]; then
  printf '%s\n\n' "$summary"
fi

section() {
  local title="$1" types="$2" lines

  lines="$(git log --no-merges --format='%h %s' "$range" | grep -E "^[0-9a-f]+ ($types)(\([^)]*\))?!?: " || true)"
  [ -n "$lines" ] || return 0

  printf '## %s\n\n' "$title"

  while read -r sha subject; do
    local scope text breaking=""

    scope="$(sed -nE 's/^[a-z]+\(([^)]*)\)!?: .*/\1/p' <<<"$subject")"
    text="$(sed -E 's/^[a-z]+(\([^)]*\))?!?: //' <<<"$subject")"

    if grep -qE '^[a-z]+(\([^)]*\))?!: ' <<<"$subject"; then
      breaking="**Breaking:** "
    fi

    printf -- '- %s%s%s ([%s](%s/commit/%s))\n' "$breaking" "${scope:+**$scope:** }" "$text" "$sha" "$repo" "$sha"
  done <<<"$lines"

  echo
}

section "New" "feat"
section "Fixes" "fix"
section "Also" "docs|perf|refactor"

version="${tag#v}"

cat <<MD
## Images

\`\`\`bash
docker pull ghcr.io/stagdhq/blobfork-azure:$version   # with Azurite inside
docker pull ghcr.io/stagdhq/blobfork-s3:$version      # with Floci inside
\`\`\`

The binaries below run beside an Azurite or Floci of your own: \`blobfork azure serve\`, \`blobfork s3 serve\`.
MD

if [ -n "$prev" ]; then
  printf '\nAll changes: %s/compare/%s...%s\n' "$repo" "$prev" "$tag"
fi
