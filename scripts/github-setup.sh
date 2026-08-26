#!/usr/bin/env bash
# Everything about the repository that is not in the repository: what GitHub
# search indexes, which security features are on, how merges behave, and
# what `main` refuses.
#
# All of it is settings-page state, which is exactly the kind of state that
# is set once, forgotten, and then quietly wrong after somebody clicks
# something. Here it is code: readable, reviewable, and safe to re-run —
# every call replaces a field wholesale rather than adding to it.
#
#   scripts/github-setup.sh              apply everything
#   scripts/github-setup.sh --show       print the current state and stop
#   PROTECTION=strict scripts/github-setup.sh
#
# Needs the gh CLI, authenticated as somebody with admin on the repository.
set -euo pipefail

REPO="${REPO:-ctolon/dynamic-config-go}"

# How main is protected. See the block further down for what each means and
# why 'solo' is the default here.
PROTECTION="${PROTECTION:-solo}"

# The job names branch protection requires. These are job *names*, not job
# ids: "CI is green" is the name of ci.yml's `ci-green`, "Security is green"
# is security.yml's `security-green`. A typo here is a branch nobody can
# merge into, so they are written once and used twice.
CHECKS='["CI is green", "Security is green"]'

say() { printf '   %s\n' "$*"; }

need() {
    command -v "$1" >/dev/null 2>&1 || { echo "$1 is required" >&2; exit 1; }
}

need gh

gh api "repos/${REPO}" >/dev/null 2>&1 || {
    echo "cannot see ${REPO} — is gh authenticated with admin on it?" >&2
    exit 1
}

show() {
    echo "── ${REPO}"

    gh api "repos/${REPO}" --jq '
        "   description: \(.description // "(none)")",
        "   homepage:    \(.homepage // "(none)")",
        "   topics:      \(.topics | join(", "))",
        "   features:    issues=\(.has_issues) wiki=\(.has_wiki) projects=\(.has_projects)",
        "   merges:      squash=\(.allow_squash_merge) rebase=\(.allow_rebase_merge) commit=\(.allow_merge_commit) auto=\(.allow_auto_merge)"'

    echo "   protection:"

    local protection

    if protection=$(gh api "repos/${REPO}/branches/main/protection" --jq '
        "     checks:   \(.required_status_checks.contexts // [] | join(", ") // "none")",
        "     admins:   \(.enforce_admins.enabled)",
        "     linear:   \(.required_linear_history.enabled)",
        "     force:    \(.allow_force_pushes.enabled)",
        "     deletion: \(.allow_deletions.enabled)"' 2>/dev/null); then
        printf '%s\n' "${protection}"
    else
        echo "     main is not protected"
    fi
}

if [ "${1:-}" = "--show" ]; then
    show
    exit 0
fi

echo "── ${REPO}"

# ------------------------------------------------------------- metadata
#
# The description and the topic list are the only text GitHub's own search
# indexes for a repository, and the homepage is what turns a repository card
# into a link to the documentation. Topics are matched exactly and capped at
# twenty, so they are lower-case, hyphenated, and specific enough that
# somebody browsing `topic:hot-reload` finds this rather than everything.

gh repo edit "${REPO}" \
    --description "Typed, validated, atomic hot reload for Viper: decode into your struct, validate before publishing, and never replace a good configuration with a bad one." \
    --homepage "https://pkg.go.dev/github.com/ctolon/dynamic-config-go" \
    --enable-issues \
    --enable-wiki=false \
    --enable-projects=false >/dev/null

say "described, homepage points at pkg.go.dev"

# --add-topic is additive and there is no --remove-all, so the REST call is
# the one that makes this idempotent.
gh api -X PUT "repos/${REPO}/topics" \
    -H "Accept: application/vnd.github+json" \
    --input - >/dev/null <<'JSON'
{"names": [
  "go", "golang", "config", "configuration", "configuration-management",
  "viper", "hot-reload", "live-reload", "settings", "yaml", "toml", "json",
  "file-watcher", "fsnotify", "kubernetes", "configmap", "twelve-factor",
  "generics", "atomic", "go-library"
]}
JSON

say "topics: go, viper, hot-reload, kubernetes, configmap, fsnotify, …"

# --------------------------------------------------------------- merges
#
# Squash and rebase only. A merge commit would make `required_linear_history`
# refuse the merge anyway, and a linear history is what lets a tag name one
# commit that means something.

gh repo edit "${REPO}" \
    --enable-squash-merge \
    --enable-rebase-merge \
    --enable-merge-commit=false \
    --enable-auto-merge \
    --delete-branch-on-merge >/dev/null

say "merges: squash and rebase, auto-merge armed, branches deleted after merge"

# ------------------------------------------------------------- security
#
# Push protection is the half that matters. Secret scanning finds a
# credential after it has been pushed — which is after it has been
# published, so the remedy is revocation rather than deletion, because the
# object is in every clone and in GitHub's event feed. Push protection
# refuses the push instead, which is the last point at which a leak is still
# a near miss.

gh api -X PUT "repos/${REPO}/vulnerability-alerts" >/dev/null
say "Dependabot alerts: on"

gh api -X PUT "repos/${REPO}/automated-security-fixes" >/dev/null
say "Dependabot security updates: on"

if gh api -X PATCH "repos/${REPO}" \
    -f 'security_and_analysis[secret_scanning][status]=enabled' \
    -f 'security_and_analysis[secret_scanning_push_protection][status]=enabled' \
    >/dev/null 2>&1; then
    say "secret scanning and push protection: on"
else
    say "::warning:: secret scanning could not be enabled (private repository?)"
fi

# ----------------------------------------------------------- protection
#
# Two shapes, because there are two honest ways to run this repository and
# the wrong one silently breaks the release script.
#
# solo   — the default, and what scripts/release.sh assumes. Both gates are
#          required, force pushes and deletion are refused, history stays
#          linear. Admins are *not* enforced, so the maintainer can push the
#          release commit straight to main. Everyone else, and every
#          Dependabot pull request, still has to be green.
#
# strict — the shape for more than one maintainer: pull requests only,
#          admins included, nobody pushes to main. Cutting a release then
#          means running release.sh on a branch, opening a pull request,
#          merging it, and pushing the tag afterwards.

case "${PROTECTION}" in
    solo)   enforce_admins=false ;;
    strict) enforce_admins=true ;;
    *)      echo "PROTECTION must be 'solo' or 'strict', not '${PROTECTION}'" >&2; exit 1 ;;
esac

gh api -X PUT "repos/${REPO}/branches/main/protection" --input - >/dev/null <<JSON
{
  "required_status_checks": { "strict": true, "contexts": ${CHECKS} },
  "enforce_admins": ${enforce_admins},
  "required_pull_request_reviews": {
    "required_approving_review_count": 0,
    "dismiss_stale_reviews": true,
    "require_code_owner_reviews": false
  },
  "restrictions": null,
  "required_linear_history": true,
  "required_conversation_resolution": true,
  "allow_force_pushes": false,
  "allow_deletions": false
}
JSON

say "main: both gates required, linear, no force pushes, no deletion (${PROTECTION})"

if [ "${PROTECTION}" = "solo" ]; then
    say "admins may still push to main, which is what release.sh does"
fi

# Tags are the releases, and a moved tag is a version the module proxy has
# already cached differently. Refusing to update or delete them makes that
# mistake impossible rather than merely discouraged.
if gh api -X POST "repos/${REPO}/rulesets" --input - >/dev/null 2>&1 <<'JSON'
{
  "name": "release tags are immutable",
  "target": "tag",
  "enforcement": "active",
  "conditions": { "ref_name": { "include": ["refs/tags/v*"], "exclude": [] } },
  "rules": [{ "type": "deletion" }, { "type": "non_fast_forward" }]
}
JSON
then
    say "tags v*: cannot be deleted or moved"
else
    say "tag ruleset already exists or could not be created — check Settings › Rules"
fi

echo
show
