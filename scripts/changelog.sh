#!/usr/bin/env bash
# Print one release's section of CHANGELOG.md.
#
#   scripts/changelog.sh 0.1.0
#   scripts/changelog.sh Unreleased
#
# The release workflow uses this for the GitHub release body, and
# release.sh uses it to prove a section exists before tagging. One reader,
# so the notes on the release and the notes in the file cannot disagree.
set -euo pipefail

version="${1:?usage: changelog.sh <version|Unreleased>}"
version="${version#v}"

changelog="$(dirname "$0")/../CHANGELOG.md"

section=$(awk -v want="$version" '
    /^## / {
        # Headings look like "## [0.1.0] - 2026-08-26" or "## [Unreleased]".
        heading = $0
        gsub(/^## \[/, "", heading)
        gsub(/\].*$/, "", heading)

        found = (heading == want)

        if (found) { printing = 1; next }
        if (printing) { exit }
    }

    # The link-reference block at the foot of the file belongs to no
    # section.
    printing && /^\[[^]]+\]: / { exit }

    printing { print }
' "$changelog")

# Trim leading and trailing blank lines.
section=$(printf '%s\n' "$section" | sed -e '/./,$!d' | tac | sed -e '/./,$!d' | tac)

if [ -z "$section" ]; then
    echo "no CHANGELOG.md section for $version" >&2
    exit 1
fi

printf '%s\n' "$section"
