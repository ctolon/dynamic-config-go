#!/usr/bin/env bash
# Prepare a release: rotate the changelog, commit, tag.
#
#   scripts/release.sh patch      0.1.0 -> 0.1.1
#   scripts/release.sh minor      0.1.0 -> 0.2.0
#   scripts/release.sh major      0.1.0 -> 1.0.0
#   scripts/release.sh 0.4.0      an explicit version
#
# A Go module has no version file — the tag is the version — so the only
# thing to edit is the changelog, and the only thing that must be true is
# that the tag, the changelog and a green tree agree. This script makes all
# three true or does nothing.
#
# It pushes nothing. Read the commit, then push the branch and the tag.
set -euo pipefail

cd "$(dirname "$0")/.."

bump="${1:?usage: release.sh <patch|minor|major|X.Y.Z>}"

die() { echo "release: $*" >&2; exit 1; }

# ---------------------------------------------------------------- checks

[ -z "$(git status --porcelain)" ] || die "working tree is dirty; commit or stash first"

branch=$(git rev-parse --abbrev-ref HEAD)

if [ "$branch" != "main" ] && [ "${ALLOW_BRANCH:-0}" != "1" ]; then
    die "on branch '$branch'; releases are cut from main (ALLOW_BRANCH=1 to override)"
fi

# ------------------------------------------------------- work out the version

current=$(awk '
    /^## \[[0-9]+\.[0-9]+\.[0-9]+\]/ {
        v = $0
        gsub(/^## \[/, "", v)
        gsub(/\].*$/, "", v)
        print v
        exit
    }
' CHANGELOG.md)

current="${current:-0.0.0}"

case "$bump" in
    patch|minor|major)
        IFS=. read -r major minor patch <<< "$current"

        case "$bump" in
            major) major=$((major + 1)); minor=0; patch=0 ;;
            minor) minor=$((minor + 1)); patch=0 ;;
            patch) patch=$((patch + 1)) ;;
        esac

        version="$major.$minor.$patch"
        ;;

    v[0-9]*|[0-9]*)
        version="${bump#v}"

        [[ "$version" =~ ^[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$ ]] ||
            die "'$bump' is not a semantic version"
        ;;

    *)
        die "'$bump' is not patch, minor, major or a version"
        ;;
esac

tag="v$version"

git rev-parse -q --verify "refs/tags/$tag" >/dev/null && die "tag $tag already exists"

# A release with no notes is a release nobody can read.
./scripts/changelog.sh Unreleased >/dev/null ||
    die "the Unreleased section of CHANGELOG.md is empty"

# ------------------------------------------------------------------ gate

echo "release: $current -> $version"
echo "release: running the full check"

make check

# ------------------------------------------------------------- changelog

today=$(date -u +%Y-%m-%d)

python3 - "$version" "$today" <<'PY'
import re
import sys

version, today = sys.argv[1], sys.argv[2]

with open("CHANGELOG.md", encoding="utf-8") as handle:
    text = handle.read()

# Rotate: Unreleased empties out, its contents become the new version's
# section.
marker = "## [Unreleased]\n"

if marker not in text:
    raise SystemExit("release: CHANGELOG.md has no Unreleased section")

text = text.replace(
    marker,
    f"{marker}\n## [{version}] - {today}\n",
    1,
)

# Refresh the link-reference block at the foot of the file.
links = re.search(r"\n(\[Unreleased\]: .*(?:\n\[.*)*)\n*\Z", text)

repository = "https://github.com/ctolon/dynamic-config-go"

previous = re.findall(r"^## \[(\d+\.\d+\.\d+)\]", text, re.MULTILINE)
previous = [v for v in previous if v != version]

if previous:
    compare = f"{repository}/compare/v{previous[0]}...v{version}"
    unreleased = f"{repository}/compare/v{version}...HEAD"
else:
    compare = f"{repository}/releases/tag/v{version}"
    unreleased = f"{repository}/compare/v{version}...HEAD"

new_link = f"[{version}]: {compare}"

if links:
    block = links.group(1)
    block = re.sub(r"^\[Unreleased\]: .*$", f"[Unreleased]: {unreleased}", block, flags=re.MULTILINE)
    block = f"{block.rstrip()}\n{new_link}\n"
    text = text[: links.start()] + "\n" + block
else:
    text = text.rstrip() + f"\n\n[Unreleased]: {unreleased}\n{new_link}\n"

with open("CHANGELOG.md", "w", encoding="utf-8") as handle:
    handle.write(text)
PY

# Prove the section the workflow will publish actually exists.
./scripts/changelog.sh "$version" >/dev/null || die "the rotated changelog has no $version section"

# ------------------------------------------------------------ commit and tag

git add CHANGELOG.md
git commit -m "release: $tag"

git tag -a "$tag" -m "$tag" -m "$(./scripts/changelog.sh "$version")"

cat <<EOF

release: prepared $tag

  Read it:    git show HEAD && git show $tag
  Publish it: git push origin $branch && git push origin $tag

Pushing the tag runs release.yml, which re-runs the gate, publishes the
GitHub release from the changelog section, and asks the module proxy to
index the version.
EOF
